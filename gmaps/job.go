package gmaps

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/uuid"
	"github.com/gosom/google-maps-scraper/deduper"
	"github.com/gosom/google-maps-scraper/exiter"
	"github.com/gosom/scrapemate"
	"github.com/playwright-community/playwright-go"
)

type GmapJobOptions func(*GmapJob)

type GmapJob struct {
	scrapemate.Job

	MaxDepth     int
	LangCode     string
	ExtractEmail bool

	Deduper             deduper.Deduper
	ExitMonitor         exiter.Exiter
	ExtractExtraReviews bool
}

func NewGmapJob(
	id, langCode, query string,
	maxDepth int,
	extractEmail bool,
	geoCoordinates string,
	zoom int,
	opts ...GmapJobOptions,
) *GmapJob {
	query = url.QueryEscape(query)

	const (
		maxRetries = 3
		prio       = scrapemate.PriorityLow
	)

	if id == "" {
		id = uuid.New().String()
	}

	mapURL := ""
	if geoCoordinates != "" && zoom > 0 {
		mapURL = fmt.Sprintf("https://www.google.com/maps/search/%s/@%s,%dz", query, strings.ReplaceAll(geoCoordinates, " ", ""), zoom)
	} else {
		//Warning: geo and zoom MUST be both set or not
		mapURL = fmt.Sprintf("https://www.google.com/maps/search/%s", query)
	}

	job := GmapJob{
		Job: scrapemate.Job{
			ID:         id,
			Method:     http.MethodGet,
			URL:        mapURL,
			URLParams:  map[string]string{"hl": langCode},
			MaxRetries: maxRetries,
			Priority:   prio,
		},
		MaxDepth:     maxDepth,
		LangCode:     langCode,
		ExtractEmail: extractEmail,
	}

	for _, opt := range opts {
		opt(&job)
	}

	return &job
}

func WithDeduper(d deduper.Deduper) GmapJobOptions {
	return func(j *GmapJob) {
		j.Deduper = d
	}
}

func WithExitMonitor(e exiter.Exiter) GmapJobOptions {
	return func(j *GmapJob) {
		j.ExitMonitor = e
	}
}

func WithExtraReviews() GmapJobOptions {
	return func(j *GmapJob) {
		j.ExtractExtraReviews = true
	}
}

func (j *GmapJob) UseInResults() bool {
	return false
}

func (j *GmapJob) Process(ctx context.Context, resp *scrapemate.Response) (any, []scrapemate.IJob, error) {
	defer func() {
		resp.Document = nil
		resp.Body = nil
	}()

	log := scrapemate.GetLoggerFromContext(ctx)

	doc, ok := resp.Document.(*goquery.Document)
	if !ok {
		return nil, nil, fmt.Errorf("could not convert to goquery document")
	}

	var next []scrapemate.IJob

	if strings.Contains(resp.URL, "/maps/place/") {
		jopts := []PlaceJobOptions{}
		if j.ExitMonitor != nil {
			jopts = append(jopts, WithPlaceJobExitMonitor(j.ExitMonitor))
		}

		placeJob := NewPlaceJob(j.ID, j.LangCode, resp.URL, j.ExtractEmail, j.ExtractExtraReviews, jopts...)

		next = append(next, placeJob)
	} else {
		doc.Find(`div[role=feed] div[jsaction]>a`).Each(func(_ int, s *goquery.Selection) {
			if href := s.AttrOr("href", ""); href != "" {
				jopts := []PlaceJobOptions{}
				if j.ExitMonitor != nil {
					jopts = append(jopts, WithPlaceJobExitMonitor(j.ExitMonitor))
				}

				nextJob := NewPlaceJob(j.ID, j.LangCode, href, j.ExtractEmail, j.ExtractExtraReviews, jopts...)

				if j.Deduper == nil || j.Deduper.AddIfNotExists(ctx, href) {
					next = append(next, nextJob)
				}
			}
		})
	}

	if j.ExitMonitor != nil {
		j.ExitMonitor.IncrPlacesFound(len(next))
		j.ExitMonitor.IncrSeedCompleted(1)
	}

	log.Info(fmt.Sprintf("%d places found", len(next)))

	return nil, next, nil
}

func (j *GmapJob) BrowserActions(ctx context.Context, page playwright.Page) scrapemate.Response {
	var resp scrapemate.Response

	// Setup debug logging
	var consoleLogs, pageErrors []string
	var consentClicked bool
	setupPageListeners(page, &consoleLogs, &pageErrors)

	pageResponse, err := page.Goto(j.GetFullURL(), playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	})

	if err != nil {
		resp.Error = err

		// Save debug info on error
		html, _ := page.Content()
		_ = saveDebugInfo(ctx, page, debugInfo{
			timestamp:      time.Now().Format(time.RFC3339),
			url:            j.GetFullURL(),
			errorMsg:       fmt.Sprintf("page.Goto failed: %v", err),
			html:           html,
			consoleLogs:    consoleLogs,
			pageErrors:     pageErrors,
			consentClicked: false,
		})

		return resp
	}

	clickErr := clickRejectCookiesIfRequired(page)
	if clickErr != nil {
		resp.Error = clickErr

		// Save debug info on consent error
		html, _ := page.Content()
		_ = saveDebugInfo(ctx, page, debugInfo{
			timestamp:      time.Now().Format(time.RFC3339),
			url:            j.GetFullURL(),
			errorMsg:       fmt.Sprintf("clickRejectCookiesIfRequired failed: %v", clickErr),
			html:           html,
			consoleLogs:    consoleLogs,
			pageErrors:     pageErrors,
			consentClicked: false,
		})

		return resp
	}

	// Track if consent was clicked (no error means either clicked or not needed)
	consentClicked = (clickErr == nil)

	const defaultTimeout = 5000

	// FIX: Don't fail if WaitForURL times out - Google Maps may redirect slowly
	// especially when using a proxy. Just log the error and continue.
	err = page.WaitForURL(page.URL(), playwright.PageWaitForURLOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(defaultTimeout),
	})

	// Intentionally ignore WaitForURL errors - we'll wait for the feed element instead
	_ = err

	resp.URL = pageResponse.URL()
	resp.StatusCode = pageResponse.Status()
	resp.Headers = make(http.Header, len(pageResponse.Headers()))

	for k, v := range pageResponse.Headers() {
		resp.Headers.Add(k, v)
	}

	// When Google Maps finds only 1 place, it slowly redirects to that place's URL
	// check element scroll
	sel := `div[role='feed']`

	//nolint:staticcheck // TODO replace with the new playwright API
	// FIX: Increased timeout from 700ms to 10000ms to allow Google Maps to load properly
	// especially when using a proxy which adds latency
	_, err = page.WaitForSelector(sel, playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(10000),
	})

	var singlePlace bool

	if err != nil {
		waitCtx, waitCancel := context.WithTimeout(ctx, time.Second*5)
		defer waitCancel()

		singlePlace = waitUntilURLContains(waitCtx, page, "/maps/place/")

		waitCancel()
	}

	if singlePlace {
		resp.URL = page.URL()

		var body string

		body, err = page.Content()
		if err != nil {
			resp.Error = err
			return resp
		}

		resp.Body = []byte(body)

		return resp
	}

	scrollSelector := `div[role='feed']`

	_, err = scroll(ctx, page, j.MaxDepth, scrollSelector)
	if err != nil {
		resp.Error = err

		// Save debug info on scroll error - THIS IS THE CRITICAL ERROR WE'RE DEBUGGING
		html, _ := page.Content()
		_ = saveDebugInfo(ctx, page, debugInfo{
			timestamp:      time.Now().Format(time.RFC3339),
			url:            j.GetFullURL(),
			errorMsg:       fmt.Sprintf("scroll failed: %v", err),
			html:           html,
			consoleLogs:    consoleLogs,
			pageErrors:     pageErrors,
			consentClicked: consentClicked,
		})

		return resp
	}

	body, err := page.Content()
	if err != nil {
		resp.Error = err
		return resp
	}

	resp.Body = []byte(body)

	return resp
}

func waitUntilURLContains(ctx context.Context, page playwright.Page, s string) bool {
	ticker := time.NewTicker(time.Millisecond * 150)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if strings.Contains(page.URL(), s) {
				return true
			}
		}
	}
}

func clickRejectCookiesIfRequired(page playwright.Page) error {
	const timeout = 5000

	// Try multiple selectors in order of preference
	// IMPORTANT: Google uses <input type="submit"> NOT <button> tags!
	selectors := []string{
		// Primary: Form-based selector targeting the "reject" submit button by value
		`input[type="submit"][value="Alle ablehnen"]`,
		`input[type="submit"][value="Reject all"]`,
		// Fallback 1: Form-based selector with aria-label
		`input[type="submit"][aria-label*="ablehnen" i]`,
		`input[type="submit"][aria-label*="reject" i]`,
		// Fallback 2: Any submit button in the consent form (first one is usually "reject")
		`form[action^="https://consent.google.com/save"] input[type="submit"]:first-of-type`,
	}

	var clickedButton playwright.ElementHandle
	var clickErr error

	// Try each selector
	for _, sel := range selectors {
		//nolint:staticcheck // TODO replace with the new playwright API
		el, err := page.WaitForSelector(sel, playwright.PageWaitForSelectorOptions{
			Timeout: playwright.Float(timeout),
		})

		if err != nil {
			// This selector didn't work, try next one
			continue
		}

		if el == nil {
			continue
		}

		// Found a button, try to click it
		//nolint:staticcheck // TODO replace with the new playwright API
		clickErr = el.Click()
		if clickErr == nil {
			clickedButton = el
			break // Successfully clicked
		}
	}

	// If we didn't find any consent button, that's OK (consent already accepted)
	if clickedButton == nil && clickErr == nil {
		return nil
	}

	// If we found a button but click failed, that's an error
	if clickErr != nil {
		return fmt.Errorf("failed to click consent reject button: %w", clickErr)
	}

	// Give Google a moment to process the consent dismissal
	time.Sleep(1000 * time.Millisecond)

	// CRITICAL FIX: Verify that consent was actually dismissed
	// If we're still on consent.google.com, the consent dialog is blocking the page
	currentURL := page.URL()
	if strings.Contains(currentURL, "consent.google.com") {
		return fmt.Errorf("consent dialog still present after click attempt (URL: %s)", currentURL)
	}

	return nil
}

func scroll(ctx context.Context,
	page playwright.Page,
	maxDepth int,
	scrollSelector string,
) (int, error) {
	// FIX: Wait for the scroll element to exist before attempting to scroll
	// This prevents the "Cannot read properties of null" error in headless mode
	// Increased timeout to 30 seconds as Google Maps can take longer to load in headless mode
	_, err := page.WaitForSelector(scrollSelector, playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(30000), // 30 seconds timeout
		State:   playwright.WaitForSelectorStateAttached,
	})
	if err != nil {
		return 0, fmt.Errorf("scroll element not found: %w", err)
	}

	expr := `async () => {
		const el = document.querySelector("` + scrollSelector + `");
		// FIX: Add null check before accessing scrollHeight
		if (!el) {
			return 0;
		}
		el.scrollTop = el.scrollHeight;

		return new Promise((resolve, reject) => {
  			setTimeout(() => {
				// FIX: Check el again after timeout
				if (!el) {
					resolve(0);
				} else {
    				resolve(el.scrollHeight);
				}
  			}, %d);
		});
	}`

	var currentScrollHeight int
	// Scroll to the bottom of the page.
	waitTime := 100.
	cnt := 0

	const (
		timeout  = 500
		maxWait2 = 2000
	)

	for i := 0; i < maxDepth; i++ {
		cnt++
		waitTime2 := timeout * cnt

		if waitTime2 > timeout {
			waitTime2 = maxWait2
		}

		// Scroll to the bottom of the page.
		scrollHeight, err := page.Evaluate(fmt.Sprintf(expr, waitTime2))
		if err != nil {
			return cnt, err
		}

		height, ok := scrollHeight.(int)
		if !ok {
			return cnt, fmt.Errorf("scrollHeight is not an int")
		}

		if height == currentScrollHeight {
			break
		}

		currentScrollHeight = height

		select {
		case <-ctx.Done():
			return currentScrollHeight, nil
		default:
		}

		waitTime *= 1.5

		if waitTime > maxWait2 {
			waitTime = maxWait2
		}

		//nolint:staticcheck // TODO replace with the new playwright API
		page.WaitForTimeout(waitTime)
	}

	return cnt, nil
}

// debugInfo holds debugging information captured during scraping
type debugInfo struct {
	timestamp      string
	url            string
	errorMsg       string
	html           string
	consoleLogs    []string
	pageErrors     []string
	consentClicked bool
}

// saveDebugInfo saves HTML content, screenshot, and logs to the debug directory
func saveDebugInfo(ctx context.Context, page playwright.Page, info debugInfo) error {
	debugDir := "/data/debug"

	// Create debug directory if it doesn't exist
	if err := os.MkdirAll(debugDir, 0755); err != nil {
		return fmt.Errorf("failed to create debug directory: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405")
	baseFilename := filepath.Join(debugDir, timestamp)

	// Save HTML content
	if info.html != "" {
		htmlFile := baseFilename + "_page.html"
		if err := os.WriteFile(htmlFile, []byte(info.html), 0644); err != nil {
			return fmt.Errorf("failed to save HTML: %w", err)
		}
	}

	// Save screenshot
	screenshotFile := baseFilename + "_screenshot.png"
	if _, err := page.Screenshot(playwright.PageScreenshotOptions{
		Path:     playwright.String(screenshotFile),
		FullPage: playwright.Bool(true),
	}); err != nil {
		// Don't fail if screenshot fails, just log it
		fmt.Fprintf(os.Stderr, "Warning: failed to save screenshot: %v\n", err)
	}

	// Save metadata and logs
	metadataFile := baseFilename + "_metadata.txt"
	metadata := fmt.Sprintf(`Timestamp: %s
URL: %s
Error: %s
Consent Clicked: %v
Page URL at error: %s

=== Console Logs ===
%s

=== Page Errors ===
%s
`,
		info.timestamp,
		info.url,
		info.errorMsg,
		info.consentClicked,
		page.URL(),
		strings.Join(info.consoleLogs, "\n"),
		strings.Join(info.pageErrors, "\n"),
	)

	if err := os.WriteFile(metadataFile, []byte(metadata), 0644); err != nil {
		return fmt.Errorf("failed to save metadata: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Debug info saved to: %s\n", debugDir)
	return nil
}

// setupPageListeners sets up console and error listeners on the page
func setupPageListeners(page playwright.Page, consoleLogs, pageErrors *[]string) {
	// Capture console messages
	page.On("console", func(msg playwright.ConsoleMessage) {
		logEntry := fmt.Sprintf("[%s] %s", msg.Type(), msg.Text())
		*consoleLogs = append(*consoleLogs, logEntry)
	})

	// Capture page errors
	page.On("pageerror", func(err error) {
		*pageErrors = append(*pageErrors, err.Error())
	})
}
