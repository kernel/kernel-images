package devtoolsproxy

// Sanitizers for the browser-control CDP commands the proxy reports. Each
// supported method has a canonical input type mirroring its parameters at
// devtools-protocol@2d019e73, and produces a separate output type generated
// from the OpenAPI schema. The split is what keeps the two jobs apart: the
// input names what the client sent, the output names what is safe to publish.
//
// The rule for every field: an argument that can carry a secret — typed and
// composition text, URLs, referrers, scripts, templates, file paths, drag
// contents, autofill values — is replaced by a length, a count, a presence
// flag, an enum or a URL scheme. Everything else is reported as it
// arrived, because an event that omits the click count or the scroll distance
// cannot answer what the agent did.
//
// Fields a canonical input type does not name are not decoded and cannot
// reach an event, so a protocol addition is privacy-safe until someone
// deliberately adds it here.

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

// sanitizer turns one command's raw params into its sanitized payload.
type sanitizer func(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error)

// sanitizers is the supported-method inventory: a method is reported if and
// only if it has an entry here.
var sanitizers = map[string]sanitizer{
	"Input.dispatchMouseEvent":         sanitizeInputDispatchMouseEvent,
	"Input.dispatchKeyEvent":           sanitizeInputDispatchKeyEvent,
	"Input.insertText":                 sanitizeInputInsertText,
	"Input.imeSetComposition":          sanitizeInputImeSetComposition,
	"Input.dispatchTouchEvent":         sanitizeInputDispatchTouchEvent,
	"Input.dispatchDragEvent":          sanitizeInputDispatchDragEvent,
	"Input.cancelDragging":             sanitizeInputCancelDragging,
	"Input.emulateTouchFromMouseEvent": sanitizeInputEmulateTouchFromMouseEvent,
	"Input.synthesizePinchGesture":     sanitizeInputSynthesizePinchGesture,
	"Input.synthesizeScrollGesture":    sanitizeInputSynthesizeScrollGesture,
	"Input.synthesizeTapGesture":       sanitizeInputSynthesizeTapGesture,
	"DOM.setFileInputFiles":            sanitizeDomSetFileInputFiles,
	"DOM.focus":                        sanitizeDomFocus,
	"DOM.scrollIntoViewIfNeeded":       sanitizeDomScrollIntoViewIfNeeded,
	"Page.bringToFront":                sanitizePageBringToFront,
	"Page.captureScreenshot":           sanitizePageCaptureScreenshot,
	"Page.captureSnapshot":             sanitizePageCaptureSnapshot,
	"Page.handleJavaScriptDialog":      sanitizePageHandleJavaScriptDialog,
	"Page.navigate":                    sanitizePageNavigate,
	"Page.navigateToHistoryEntry":      sanitizePageNavigateToHistoryEntry,
	"Page.reload":                      sanitizePageReload,
	"Page.printToPDF":                  sanitizePagePrintToPDF,
	"Page.startScreencast":             sanitizePageStartScreencast,
	"Page.stopScreencast":              sanitizePageStopScreencast,
	"Page.stopLoading":                 sanitizePageStopLoading,
	"Page.close":                       sanitizePageClose,
	"Page.setWebLifecycleState":        sanitizePageSetWebLifecycleState,
	"Target.activateTarget":            sanitizeTargetActivateTarget,
	"Target.closeTarget":               sanitizeTargetCloseTarget,
	"Target.createTarget":              sanitizeTargetCreateTarget,
	"Target.createBrowserContext":      sanitizeTargetCreateBrowserContext,
	"Target.disposeBrowserContext":     sanitizeTargetDisposeBrowserContext,
	"Target.openDevTools":              sanitizeTargetOpenDevTools,
	"Browser.cancelDownload":           sanitizeBrowserCancelDownload,
	"Browser.close":                    sanitizeBrowserClose,
	"Browser.setWindowBounds":          sanitizeBrowserSetWindowBounds,
	"Browser.setContentsSize":          sanitizeBrowserSetContentsSize,
	"Autofill.trigger":                 sanitizeAutofillTrigger,
}

// namedKeys are the KeyboardEvent.key values worth reading back: keys that
// command the page rather than type into it. This is an allowlist rather than a
// "more than one character" rule because key for typed input can itself be
// multi-rune — a decomposed "é" is two runes and is the letter someone typed,
// so a length rule would publish it.
var namedKeys = lookup(`
	Enter Tab Escape Backspace Delete Insert
	Home End PageUp PageDown ArrowUp ArrowDown ArrowLeft ArrowRight
	Shift Control Alt Meta CapsLock NumLock ScrollLock
	ContextMenu Pause PrintScreen
	F1 F2 F3 F4 F5 F6 F7 F8 F9 F10 F11 F12
`)

// mimeCategories are the top-level MIME types a drag payload may report. A
// subtype names the file ("application/vnd.acme.invoice-2024"), so only the
// category survives, and one outside this set reports as "other".
var mimeCategories = lookup(`text image audio video application font model multipart message`)

// lookup builds a membership set from a whitespace-separated list, so the lists
// above read as lists.
func lookup(words string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, word := range strings.Fields(words) {
		out[word] = struct{}{}
	}
	return out
}

// decodeParams fills p from a command's params. Several control commands take
// no arguments, so an absent params object means "all defaults", not an error.
func decodeParams(raw json.RawMessage, p any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, p)
}

func runeLen(s *string) *int {
	if s == nil {
		return nil
	}
	n := utf8.RuneCountInString(*s)
	return &n
}

func present(s *string) *bool {
	p := s != nil && *s != ""
	return &p
}

func count[T any](items []T) *int {
	n := len(items)
	return &n
}

func boolPtr(b bool) *bool { return &b }

// namedKey passes through only a key that commands the page. A key that
// produces a character is the character someone typed.
func namedKey(key *string) *string {
	if key == nil {
		return nil
	}
	if _, ok := namedKeys[*key]; !ok {
		return nil
	}
	return key
}

// urlScheme reduces a URL to its scheme. The host names the site the agent
// went to and the path and query can carry a reset token, so neither leaves
// the VM through the control category; the page category is where a reader
// opts in to navigation URLs.
func urlScheme(raw string) *string {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		return nil
	}
	scheme := parsed.Scheme
	return &scheme
}

// ---- Input ----

type inputDispatchMouseEventParams struct {
	Type               string   `json:"type"`
	X                  *float64 `json:"x"`
	Y                  *float64 `json:"y"`
	Modifiers          *int     `json:"modifiers"`
	Button             *string  `json:"button"`
	Buttons            *int     `json:"buttons"`
	ClickCount         *int     `json:"clickCount"`
	Force              *float64 `json:"force"`
	TangentialPressure *float64 `json:"tangentialPressure"`
	TiltX              *float64 `json:"tiltX"`
	TiltY              *float64 `json:"tiltY"`
	Twist              *int     `json:"twist"`
	DeltaX             *float64 `json:"deltaX"`
	DeltaY             *float64 `json:"deltaY"`
	PointerType        *string  `json:"pointerType"`
}

func sanitizeInputDispatchMouseEvent(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputDispatchMouseEventParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpInputDispatchMouseEventCommandData(oapi.BrowserCdpInputDispatchMouseEventCommandData{
		SessionId:          cmd.sessionID(),
		EventType:          p.Type,
		X:                  p.X,
		Y:                  p.Y,
		Modifiers:          p.Modifiers,
		Button:             p.Button,
		Buttons:            p.Buttons,
		ClickCount:         p.ClickCount,
		DeltaX:             p.DeltaX,
		DeltaY:             p.DeltaY,
		PointerType:        p.PointerType,
		Force:              p.Force,
		TangentialPressure: p.TangentialPressure,
		TiltX:              p.TiltX,
		TiltY:              p.TiltY,
		Twist:              p.Twist,
	})
}

type inputDispatchKeyEventParams struct {
	Type        string   `json:"type"`
	Modifiers   *int     `json:"modifiers"`
	Text        *string  `json:"text"`
	Key         *string  `json:"key"`
	Location    *int     `json:"location"`
	AutoRepeat  *bool    `json:"autoRepeat"`
	IsKeypad    *bool    `json:"isKeypad"`
	IsSystemKey *bool    `json:"isSystemKey"`
	Commands    []string `json:"commands"`
}

func sanitizeInputDispatchKeyEvent(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputDispatchKeyEventParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	data := oapi.BrowserCdpInputDispatchKeyEventCommandData{
		SessionId:   cmd.sessionID(),
		EventType:   p.Type,
		Modifiers:   p.Modifiers,
		TextLength:  runeLen(p.Text),
		NamedKey:    namedKey(p.Key),
		Location:    p.Location,
		AutoRepeat:  p.AutoRepeat,
		IsKeypad:    p.IsKeypad,
		IsSystemKey: p.IsSystemKey,
	}
	// code, keyIdentifier and the virtual key codes all name the character as
	// surely as text does, so they are never decoded.
	if p.Commands != nil {
		data.CommandCount = count(p.Commands)
	}
	return out, out.FromBrowserCdpInputDispatchKeyEventCommandData(data)
}

type inputInsertTextParams struct {
	Text string `json:"text"`
}

func sanitizeInputInsertText(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputInsertTextParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpInputInsertTextCommandData(oapi.BrowserCdpInputInsertTextCommandData{
		SessionId:  cmd.sessionID(),
		TextLength: utf8.RuneCountInString(p.Text),
	})
}

type inputImeSetCompositionParams struct {
	Text             string `json:"text"`
	SelectionStart   *int   `json:"selectionStart"`
	SelectionEnd     *int   `json:"selectionEnd"`
	ReplacementStart *int   `json:"replacementStart"`
	ReplacementEnd   *int   `json:"replacementEnd"`
}

func sanitizeInputImeSetComposition(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputImeSetCompositionParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpInputImeSetCompositionCommandData(oapi.BrowserCdpInputImeSetCompositionCommandData{
		SessionId:        cmd.sessionID(),
		TextLength:       utf8.RuneCountInString(p.Text),
		SelectionStart:   p.SelectionStart,
		SelectionEnd:     p.SelectionEnd,
		ReplacementStart: p.ReplacementStart,
		ReplacementEnd:   p.ReplacementEnd,
	})
}

type touchPoint struct {
	X *float64 `json:"x"`
	Y *float64 `json:"y"`
}

type inputDispatchTouchEventParams struct {
	Type        string       `json:"type"`
	TouchPoints []touchPoint `json:"touchPoints"`
	Modifiers   *int         `json:"modifiers"`
}

func sanitizeInputDispatchTouchEvent(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputDispatchTouchEventParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	data := oapi.BrowserCdpInputDispatchTouchEventCommandData{
		SessionId:       cmd.sessionID(),
		EventType:       p.Type,
		TouchPointCount: len(p.TouchPoints),
		Modifiers:       p.Modifiers,
	}
	// A touch dispatch carries its coordinates inside touchPoints rather than
	// at the top level, so the primary point stands in for where it landed.
	if len(p.TouchPoints) > 0 {
		data.X = p.TouchPoints[0].X
		data.Y = p.TouchPoints[0].Y
	}
	return out, out.FromBrowserCdpInputDispatchTouchEventCommandData(data)
}

type dragDataItem struct {
	MimeType string `json:"mimeType"`
}

type dragData struct {
	Items              []dragDataItem `json:"items"`
	Files              []string       `json:"files"`
	DragOperationsMask *int           `json:"dragOperationsMask"`
}

type inputDispatchDragEventParams struct {
	Type      string   `json:"type"`
	X         *float64 `json:"x"`
	Y         *float64 `json:"y"`
	Modifiers *int     `json:"modifiers"`
	Data      dragData `json:"data"`
}

func sanitizeInputDispatchDragEvent(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputDispatchDragEventParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	data := oapi.BrowserCdpInputDispatchDragEventCommandData{
		SessionId:          cmd.sessionID(),
		EventType:          p.Type,
		X:                  p.X,
		Y:                  p.Y,
		Modifiers:          p.Modifiers,
		DragItemCount:      count(p.Data.Items),
		DragFileCount:      count(p.Data.Files),
		DragOperationsMask: p.Data.DragOperationsMask,
	}
	if cats := mimeCategoriesOf(p.Data.Items); len(cats) > 0 {
		data.DragMimeCategories = &cats
	}
	return out, out.FromBrowserCdpInputDispatchDragEventCommandData(data)
}

// mimeCategoriesOf reduces drag item MIME types to their distinct top-level
// categories. The subtype names the file, so it does not survive.
func mimeCategoriesOf(items []dragDataItem) []string {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		category, _, _ := strings.Cut(item.MimeType, "/")
		category = strings.ToLower(strings.TrimSpace(category))
		if _, ok := mimeCategories[category]; !ok {
			category = "other"
		}
		seen[category] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for category := range seen {
		out = append(out, category)
	}
	sort.Strings(out)
	return out
}

func sanitizeInputCancelDragging(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpInputCancelDraggingCommandData(oapi.BrowserCdpInputCancelDraggingCommandData{
		SessionId: cmd.sessionID(),
	})
}

type inputEmulateTouchFromMouseEventParams struct {
	Type       string   `json:"type"`
	X          *float64 `json:"x"`
	Y          *float64 `json:"y"`
	Button     *string  `json:"button"`
	Modifiers  *int     `json:"modifiers"`
	ClickCount *int     `json:"clickCount"`
	DeltaX     *float64 `json:"deltaX"`
	DeltaY     *float64 `json:"deltaY"`
}

func sanitizeInputEmulateTouchFromMouseEvent(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputEmulateTouchFromMouseEventParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpInputEmulateTouchFromMouseEventCommandData(oapi.BrowserCdpInputEmulateTouchFromMouseEventCommandData{
		SessionId:  cmd.sessionID(),
		EventType:  p.Type,
		X:          p.X,
		Y:          p.Y,
		Button:     p.Button,
		Modifiers:  p.Modifiers,
		ClickCount: p.ClickCount,
		DeltaX:     p.DeltaX,
		DeltaY:     p.DeltaY,
	})
}

type inputSynthesizePinchGestureParams struct {
	X                 *float64 `json:"x"`
	Y                 *float64 `json:"y"`
	ScaleFactor       *float64 `json:"scaleFactor"`
	RelativeSpeed     *int     `json:"relativeSpeed"`
	GestureSourceType *string  `json:"gestureSourceType"`
}

func sanitizeInputSynthesizePinchGesture(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputSynthesizePinchGestureParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpInputSynthesizePinchGestureCommandData(oapi.BrowserCdpInputSynthesizePinchGestureCommandData{
		SessionId:         cmd.sessionID(),
		X:                 p.X,
		Y:                 p.Y,
		ScaleFactor:       p.ScaleFactor,
		RelativeSpeed:     p.RelativeSpeed,
		GestureSourceType: p.GestureSourceType,
	})
}

type inputSynthesizeScrollGestureParams struct {
	X                 *float64 `json:"x"`
	Y                 *float64 `json:"y"`
	XDistance         *float64 `json:"xDistance"`
	YDistance         *float64 `json:"yDistance"`
	XOverscroll       *float64 `json:"xOverscroll"`
	YOverscroll       *float64 `json:"yOverscroll"`
	PreventFling      *bool    `json:"preventFling"`
	Speed             *int     `json:"speed"`
	GestureSourceType *string  `json:"gestureSourceType"`
	RepeatCount       *int     `json:"repeatCount"`
	RepeatDelayMs     *int     `json:"repeatDelayMs"`
}

func sanitizeInputSynthesizeScrollGesture(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputSynthesizeScrollGestureParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	// interactionMarkerName is a caller-supplied label, so it is not decoded.
	return out, out.FromBrowserCdpInputSynthesizeScrollGestureCommandData(oapi.BrowserCdpInputSynthesizeScrollGestureCommandData{
		SessionId:         cmd.sessionID(),
		X:                 p.X,
		Y:                 p.Y,
		XDistance:         p.XDistance,
		YDistance:         p.YDistance,
		XOverscroll:       p.XOverscroll,
		YOverscroll:       p.YOverscroll,
		PreventFling:      p.PreventFling,
		Speed:             p.Speed,
		GestureSourceType: p.GestureSourceType,
		RepeatCount:       p.RepeatCount,
		RepeatDelayMs:     p.RepeatDelayMs,
	})
}

type inputSynthesizeTapGestureParams struct {
	X                 *float64 `json:"x"`
	Y                 *float64 `json:"y"`
	Duration          *int     `json:"duration"`
	TapCount          *int     `json:"tapCount"`
	GestureSourceType *string  `json:"gestureSourceType"`
}

func sanitizeInputSynthesizeTapGesture(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p inputSynthesizeTapGestureParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpInputSynthesizeTapGestureCommandData(oapi.BrowserCdpInputSynthesizeTapGestureCommandData{
		SessionId:         cmd.sessionID(),
		X:                 p.X,
		Y:                 p.Y,
		Duration:          p.Duration,
		TapCount:          p.TapCount,
		GestureSourceType: p.GestureSourceType,
	})
}

// ---- DOM ----

// domNodeRef is the three-way node reference DOM commands take. It is shared
// because it is one canonical argument group, not because the commands are.
type domNodeRef struct {
	NodeId        *int    `json:"nodeId"`
	BackendNodeId *int    `json:"backendNodeId"`
	ObjectId      *string `json:"objectId"`
}

type domSetFileInputFilesParams struct {
	domNodeRef
	Files []string `json:"files"`
}

func sanitizeDomSetFileInputFiles(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p domSetFileInputFilesParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpDomSetFileInputFilesCommandData(oapi.BrowserCdpDomSetFileInputFilesCommandData{
		SessionId:     cmd.sessionID(),
		FileCount:     len(p.Files),
		NodeId:        p.NodeId,
		BackendNodeId: p.BackendNodeId,
		ObjectId:      p.ObjectId,
	})
}

func sanitizeDomFocus(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p domNodeRef
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpDomFocusCommandData(oapi.BrowserCdpDomFocusCommandData{
		SessionId:     cmd.sessionID(),
		NodeId:        p.NodeId,
		BackendNodeId: p.BackendNodeId,
		ObjectId:      p.ObjectId,
	})
}

type domScrollIntoViewIfNeededParams struct {
	domNodeRef
	Rect json.RawMessage `json:"rect"`
}

func sanitizeDomScrollIntoViewIfNeeded(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p domScrollIntoViewIfNeededParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpDomScrollIntoViewIfNeededCommandData(oapi.BrowserCdpDomScrollIntoViewIfNeededCommandData{
		SessionId:     cmd.sessionID(),
		NodeId:        p.NodeId,
		BackendNodeId: p.BackendNodeId,
		ObjectId:      p.ObjectId,
		HasRect:       boolPtr(len(p.Rect) > 0 && string(p.Rect) != "null"),
	})
}

// ---- Page ----

func sanitizePageBringToFront(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpPageBringToFrontCommandData(oapi.BrowserCdpPageBringToFrontCommandData{
		SessionId: cmd.sessionID(),
	})
}

type viewport struct {
	X      *float64 `json:"x"`
	Y      *float64 `json:"y"`
	Width  *float64 `json:"width"`
	Height *float64 `json:"height"`
	Scale  *float64 `json:"scale"`
}

type pageCaptureScreenshotParams struct {
	Format                *string   `json:"format"`
	Quality               *int      `json:"quality"`
	Clip                  *viewport `json:"clip"`
	FromSurface           *bool     `json:"fromSurface"`
	CaptureBeyondViewport *bool     `json:"captureBeyondViewport"`
	OptimizeForSpeed      *bool     `json:"optimizeForSpeed"`
}

func sanitizePageCaptureScreenshot(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pageCaptureScreenshotParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	data := oapi.BrowserCdpPageCaptureScreenshotCommandData{
		SessionId:             cmd.sessionID(),
		Format:                p.Format,
		Quality:               p.Quality,
		FromSurface:           p.FromSurface,
		CaptureBeyondViewport: p.CaptureBeyondViewport,
		OptimizeForSpeed:      p.OptimizeForSpeed,
	}
	if p.Clip != nil {
		data.ClipX, data.ClipY = p.Clip.X, p.Clip.Y
		data.ClipWidth, data.ClipHeight, data.ClipScale = p.Clip.Width, p.Clip.Height, p.Clip.Scale
	}
	return out, out.FromBrowserCdpPageCaptureScreenshotCommandData(data)
}

type pageCaptureSnapshotParams struct {
	Format *string `json:"format"`
}

func sanitizePageCaptureSnapshot(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pageCaptureSnapshotParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpPageCaptureSnapshotCommandData(oapi.BrowserCdpPageCaptureSnapshotCommandData{
		SessionId: cmd.sessionID(),
		Format:    p.Format,
	})
}

type pageHandleJavaScriptDialogParams struct {
	Accept     bool    `json:"accept"`
	PromptText *string `json:"promptText"`
}

func sanitizePageHandleJavaScriptDialog(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pageHandleJavaScriptDialogParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpPageHandleJavaScriptDialogCommandData(oapi.BrowserCdpPageHandleJavaScriptDialogCommandData{
		SessionId:        cmd.sessionID(),
		Accept:           p.Accept,
		PromptTextLength: runeLen(p.PromptText),
	})
}

type pageNavigateParams struct {
	Url            string  `json:"url"`
	Referrer       *string `json:"referrer"`
	TransitionType *string `json:"transitionType"`
	FrameId        *string `json:"frameId"`
	ReferrerPolicy *string `json:"referrerPolicy"`
}

func sanitizePageNavigate(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pageNavigateParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpPageNavigateCommandData(oapi.BrowserCdpPageNavigateCommandData{
		SessionId:       cmd.sessionID(),
		UrlScheme:       urlScheme(p.Url),
		TransitionType:  p.TransitionType,
		ReferrerPresent: present(p.Referrer),
		ReferrerPolicy:  p.ReferrerPolicy,
		FrameId:         p.FrameId,
	})
}

type pageNavigateToHistoryEntryParams struct {
	EntryId int `json:"entryId"`
}

func sanitizePageNavigateToHistoryEntry(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pageNavigateToHistoryEntryParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpPageNavigateToHistoryEntryCommandData(oapi.BrowserCdpPageNavigateToHistoryEntryCommandData{
		SessionId: cmd.sessionID(),
		EntryId:   p.EntryId,
	})
}

type pageReloadParams struct {
	IgnoreCache            *bool   `json:"ignoreCache"`
	ScriptToEvaluateOnLoad *string `json:"scriptToEvaluateOnLoad"`
	LoaderId               *string `json:"loaderId"`
}

func sanitizePageReload(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pageReloadParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpPageReloadCommandData(oapi.BrowserCdpPageReloadCommandData{
		SessionId:     cmd.sessionID(),
		IgnoreCache:   p.IgnoreCache,
		ScriptPresent: present(p.ScriptToEvaluateOnLoad),
		ScriptLength:  runeLen(p.ScriptToEvaluateOnLoad),
		LoaderId:      p.LoaderId,
	})
}

type pagePrintToPDFParams struct {
	Landscape           *bool    `json:"landscape"`
	DisplayHeaderFooter *bool    `json:"displayHeaderFooter"`
	PrintBackground     *bool    `json:"printBackground"`
	Scale               *float64 `json:"scale"`
	PaperWidth          *float64 `json:"paperWidth"`
	PaperHeight         *float64 `json:"paperHeight"`
	PageRanges          *string  `json:"pageRanges"`
	HeaderTemplate      *string  `json:"headerTemplate"`
	FooterTemplate      *string  `json:"footerTemplate"`
	PreferCSSPageSize   *bool    `json:"preferCSSPageSize"`
	TransferMode        *string  `json:"transferMode"`
}

func sanitizePagePrintToPDF(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pagePrintToPDFParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpPagePrintToPdfCommandData(oapi.BrowserCdpPagePrintToPdfCommandData{
		SessionId:             cmd.sessionID(),
		Landscape:             p.Landscape,
		Scale:                 p.Scale,
		PaperWidth:            p.PaperWidth,
		PaperHeight:           p.PaperHeight,
		DisplayHeaderFooter:   p.DisplayHeaderFooter,
		PrintBackground:       p.PrintBackground,
		PreferCssPageSize:     p.PreferCSSPageSize,
		TransferMode:          p.TransferMode,
		PageRangesPresent:     present(p.PageRanges),
		HeaderTemplatePresent: present(p.HeaderTemplate),
		FooterTemplatePresent: present(p.FooterTemplate),
	})
}

type pageStartScreencastParams struct {
	Format        *string `json:"format"`
	Quality       *int    `json:"quality"`
	MaxWidth      *int    `json:"maxWidth"`
	MaxHeight     *int    `json:"maxHeight"`
	EveryNthFrame *int    `json:"everyNthFrame"`
}

func sanitizePageStartScreencast(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pageStartScreencastParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpPageStartScreencastCommandData(oapi.BrowserCdpPageStartScreencastCommandData{
		SessionId:     cmd.sessionID(),
		Format:        p.Format,
		Quality:       p.Quality,
		MaxWidth:      p.MaxWidth,
		MaxHeight:     p.MaxHeight,
		EveryNthFrame: p.EveryNthFrame,
	})
}

func sanitizePageStopScreencast(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpPageStopScreencastCommandData(oapi.BrowserCdpPageStopScreencastCommandData{
		SessionId: cmd.sessionID(),
	})
}

func sanitizePageStopLoading(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpPageStopLoadingCommandData(oapi.BrowserCdpPageStopLoadingCommandData{
		SessionId: cmd.sessionID(),
	})
}

func sanitizePageClose(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpPageCloseCommandData(oapi.BrowserCdpPageCloseCommandData{
		SessionId: cmd.sessionID(),
	})
}

type pageSetWebLifecycleStateParams struct {
	State string `json:"state"`
}

func sanitizePageSetWebLifecycleState(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pageSetWebLifecycleStateParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpPageSetWebLifecycleStateCommandData(oapi.BrowserCdpPageSetWebLifecycleStateCommandData{
		SessionId: cmd.sessionID(),
		State:     p.State,
	})
}

// ---- Target ----

type targetIdParams struct {
	TargetId string `json:"targetId"`
	PanelId  string `json:"panelId"`
}

func sanitizeTargetActivateTarget(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p targetIdParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpTargetActivateTargetCommandData(oapi.BrowserCdpTargetActivateTargetCommandData{
		SessionId: cmd.sessionID(),
		TargetId:  p.TargetId,
	})
}

func sanitizeTargetCloseTarget(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p targetIdParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpTargetCloseTargetCommandData(oapi.BrowserCdpTargetCloseTargetCommandData{
		SessionId: cmd.sessionID(),
		TargetId:  p.TargetId,
	})
}

func sanitizeTargetOpenDevTools(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p targetIdParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	data := oapi.BrowserCdpTargetOpenDevToolsCommandData{
		SessionId: cmd.sessionID(),
		TargetId:  p.TargetId,
	}
	if p.PanelId != "" {
		data.PanelId = &p.PanelId
	}
	return out, out.FromBrowserCdpTargetOpenDevToolsCommandData(data)
}

type targetCreateTargetParams struct {
	Url                     string  `json:"url"`
	Left                    *int    `json:"left"`
	Top                     *int    `json:"top"`
	Width                   *int    `json:"width"`
	Height                  *int    `json:"height"`
	WindowState             *string `json:"windowState"`
	BrowserContextId        *string `json:"browserContextId"`
	EnableBeginFrameControl *bool   `json:"enableBeginFrameControl"`
	NewWindow               *bool   `json:"newWindow"`
	Background              *bool   `json:"background"`
	ForTab                  *bool   `json:"forTab"`
	Hidden                  *bool   `json:"hidden"`
}

func sanitizeTargetCreateTarget(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p targetCreateTargetParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpTargetCreateTargetCommandData(oapi.BrowserCdpTargetCreateTargetCommandData{
		SessionId:               cmd.sessionID(),
		UrlScheme:               urlScheme(p.Url),
		Left:                    p.Left,
		Top:                     p.Top,
		Width:                   p.Width,
		Height:                  p.Height,
		WindowState:             p.WindowState,
		BrowserContextId:        p.BrowserContextId,
		NewWindow:               p.NewWindow,
		Background:              p.Background,
		ForTab:                  p.ForTab,
		Hidden:                  p.Hidden,
		EnableBeginFrameControl: p.EnableBeginFrameControl,
	})
}

type targetCreateBrowserContextParams struct {
	DisposeOnDetach                   *bool    `json:"disposeOnDetach"`
	ProxyServer                       *string  `json:"proxyServer"`
	ProxyBypassList                   *string  `json:"proxyBypassList"`
	OriginsWithUniversalNetworkAccess []string `json:"originsWithUniversalNetworkAccess"`
}

func sanitizeTargetCreateBrowserContext(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p targetCreateBrowserContextParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpTargetCreateBrowserContextCommandData(oapi.BrowserCdpTargetCreateBrowserContextCommandData{
		SessionId:                         cmd.sessionID(),
		DisposeOnDetach:                   p.DisposeOnDetach,
		ProxyServerPresent:                present(p.ProxyServer),
		ProxyBypassListPresent:            present(p.ProxyBypassList),
		UniversalNetworkAccessOriginCount: count(p.OriginsWithUniversalNetworkAccess),
	})
}

type browserContextIdParams struct {
	BrowserContextId string `json:"browserContextId"`
}

func sanitizeTargetDisposeBrowserContext(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p browserContextIdParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpTargetDisposeBrowserContextCommandData(oapi.BrowserCdpTargetDisposeBrowserContextCommandData{
		SessionId:        cmd.sessionID(),
		BrowserContextId: p.BrowserContextId,
	})
}

// ---- Browser ----

type browserCancelDownloadParams struct {
	Guid             string  `json:"guid"`
	BrowserContextId *string `json:"browserContextId"`
}

func sanitizeBrowserCancelDownload(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p browserCancelDownloadParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpBrowserCancelDownloadCommandData(oapi.BrowserCdpBrowserCancelDownloadCommandData{
		SessionId:        cmd.sessionID(),
		DownloadGuid:     p.Guid,
		BrowserContextId: p.BrowserContextId,
	})
}

func sanitizeBrowserClose(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpBrowserCloseCommandData(oapi.BrowserCdpBrowserCloseCommandData{
		SessionId: cmd.sessionID(),
	})
}

type windowBounds struct {
	Left        *int    `json:"left"`
	Top         *int    `json:"top"`
	Width       *int    `json:"width"`
	Height      *int    `json:"height"`
	WindowState *string `json:"windowState"`
}

type browserSetWindowBoundsParams struct {
	WindowId int          `json:"windowId"`
	Bounds   windowBounds `json:"bounds"`
}

func sanitizeBrowserSetWindowBounds(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p browserSetWindowBoundsParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpBrowserSetWindowBoundsCommandData(oapi.BrowserCdpBrowserSetWindowBoundsCommandData{
		SessionId:   cmd.sessionID(),
		WindowId:    p.WindowId,
		Left:        p.Bounds.Left,
		Top:         p.Bounds.Top,
		Width:       p.Bounds.Width,
		Height:      p.Bounds.Height,
		WindowState: p.Bounds.WindowState,
	})
}

type browserSetContentsSizeParams struct {
	WindowId int  `json:"windowId"`
	Width    *int `json:"width"`
	Height   *int `json:"height"`
}

func sanitizeBrowserSetContentsSize(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p browserSetContentsSizeParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpBrowserSetContentsSizeCommandData(oapi.BrowserCdpBrowserSetContentsSizeCommandData{
		SessionId: cmd.sessionID(),
		WindowId:  p.WindowId,
		Width:     p.Width,
		Height:    p.Height,
	})
}

// ---- Autofill ----

type autofillTriggerParams struct {
	FieldId int             `json:"fieldId"`
	FrameId *string         `json:"frameId"`
	Card    json.RawMessage `json:"card"`
	Address json.RawMessage `json:"address"`
}

func sanitizeAutofillTrigger(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p autofillTriggerParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	data := oapi.BrowserCdpAutofillTriggerCommandData{
		SessionId: cmd.sessionID(),
		FieldId:   p.FieldId,
		FrameId:   p.FrameId,
	}
	// The card number and the address lines are the whole payload, so only
	// which of the two was filled survives.
	switch {
	case len(p.Card) > 0 && string(p.Card) != "null":
		mode := "card"
		data.Mode = &mode
	case len(p.Address) > 0 && string(p.Address) != "null":
		mode := "address"
		data.Mode = &mode
	}
	return out, out.FromBrowserCdpAutofillTriggerCommandData(data)
}
