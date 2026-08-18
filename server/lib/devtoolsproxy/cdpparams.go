package devtoolsproxy

// Sanitizers for the browser-control CDP commands the proxy reports. Each
// supported method has a canonical input type mirroring its parameters, and
// produces a separate output type generated from the OpenAPI schema. The split
// is what keeps the two jobs apart: the input names what the client sent, the
// output names what is safe to publish.
//
// The canonical definitions are devtools-protocol at 2d019e73, pinned here:
// https://github.com/ChromeDevTools/devtools-protocol/blob/2d019e73eb371d1d6985d26d395d78bd8f8a22ba/json/browser_protocol.json
//
// The rule for every field: an argument that can carry a secret — typed and
// composition text, URLs, referrers, scripts, templates, file paths, drag
// contents, autofill values — is replaced by a length, a count, a presence
// flag, an enum or a URL scheme. Everything else is reported as it arrived,
// because an event that omits the click count or the scroll distance cannot
// answer what the agent did.
//
// Fields a canonical input type does not name are not decoded and cannot reach
// an event, so a protocol addition is privacy-safe until someone deliberately
// adds it here. Which arguments those are is not left implicit: every argument
// of every supported command carries a retained or redacted decision in
// testdata/cdp_arguments.yaml, checked against a snapshot of the pinned
// protocol by the tests in cdpmanifest_test.go.

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

// commandParams is the canonical input type each supported method decodes its
// arguments into. It exists so the drift check can compare what the code reads
// against testdata/cdp_arguments.yaml: an argument decoded here but not
// declared there, or declared retained but never decoded, is a disagreement
// between the sanitizers and the record of what they are meant to do.
var commandParams = map[string]any{
	"Input.dispatchMouseEvent":         inputDispatchMouseEventParams{},
	"Input.dispatchKeyEvent":           inputDispatchKeyEventParams{},
	"Input.insertText":                 inputInsertTextParams{},
	"Input.imeSetComposition":          inputImeSetCompositionParams{},
	"Input.dispatchTouchEvent":         inputDispatchTouchEventParams{},
	"Input.dispatchDragEvent":          inputDispatchDragEventParams{},
	"Input.cancelDragging":             struct{}{},
	"Input.emulateTouchFromMouseEvent": inputEmulateTouchFromMouseEventParams{},
	"Input.synthesizePinchGesture":     inputSynthesizePinchGestureParams{},
	"Input.synthesizeScrollGesture":    inputSynthesizeScrollGestureParams{},
	"Input.synthesizeTapGesture":       inputSynthesizeTapGestureParams{},
	"DOM.setFileInputFiles":            domSetFileInputFilesParams{},
	"DOM.focus":                        domNodeRef{},
	"DOM.scrollIntoViewIfNeeded":       domScrollIntoViewIfNeededParams{},
	"Page.bringToFront":                struct{}{},
	"Page.captureScreenshot":           pageCaptureScreenshotParams{},
	"Page.captureSnapshot":             pageCaptureSnapshotParams{},
	"Page.handleJavaScriptDialog":      pageHandleJavaScriptDialogParams{},
	"Page.navigate":                    pageNavigateParams{},
	"Page.navigateToHistoryEntry":      pageNavigateToHistoryEntryParams{},
	"Page.reload":                      pageReloadParams{},
	"Page.printToPDF":                  pagePrintToPDFParams{},
	"Page.startScreencast":             pageStartScreencastParams{},
	"Page.stopScreencast":              struct{}{},
	"Page.stopLoading":                 struct{}{},
	"Page.close":                       struct{}{},
	"Page.setWebLifecycleState":        pageSetWebLifecycleStateParams{},
	"Target.activateTarget":            targetIdParams{},
	"Target.closeTarget":               targetIdParams{},
	"Target.createTarget":              targetCreateTargetParams{},
	"Target.createBrowserContext":      targetCreateBrowserContextParams{},
	"Target.disposeBrowserContext":     browserContextIdParams{},
	"Target.openDevTools":              targetOpenDevToolsParams{},
	"Browser.cancelDownload":           browserCancelDownloadParams{},
	"Browser.close":                    struct{}{},
	"Browser.setWindowBounds":          browserSetWindowBoundsParams{},
	"Browser.setContentsSize":          browserSetContentsSizeParams{},
	"Autofill.trigger":                 autofillTriggerParams{},
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

// A client controls the enum strings below, and an event is only as bounded as
// the values it copies: a 1 MB button would push the payload past the envelope
// limit, and truncateIfNeeded drops the whole event rather than the field. So a
// value is passed through only when the generated Valid() — the spec's own enum
// membership — accepts it, and is reported as "other" otherwise.
const unknownEnumValue = "other"

// maxOpaqueIDBytes bounds identifiers the protocol leaves as free-form strings.
// Real ones are well under this; anything longer is a broken or hostile client,
// and clipping keeps one from taking the event down with it.
const maxOpaqueIDBytes = 128

// enumValue is a generated enum type that can vet its own value.
type enumValue interface {
	~string
	Valid() bool
}

// enumOf narrows a client string to a generated enum, for a field the schema
// always carries. An empty or unrecognised value maps to the "other" fallback
// so the emitted payload is always schema-valid.
func enumOf[T enumValue](v string) T {
	if v != "" {
		if out := T(v); out.Valid() {
			return out
		}
	}
	return T(unknownEnumValue)
}

// optionalEnumOf is enumOf for a field the schema omits when absent.
func optionalEnumOf[T enumValue](v *string) *T {
	if v == nil || *v == "" {
		return nil
	}
	out := enumOf[T](*v)
	return &out
}

// clipID bounds an opaque identifier. Clipping rather than dropping keeps a
// required field present and a long-but-real id partially readable.
func clipID(v string) string {
	if len(v) <= maxOpaqueIDBytes {
		return v
	}
	return v[:maxOpaqueIDBytes]
}

func clipIDPtr(v *string) *string {
	if v == nil {
		return nil
	}
	clipped := clipID(*v)
	return &clipped
}

func mouseEventType(v string) oapi.BrowserCdpMouseEventType {
	return enumOf[oapi.BrowserCdpMouseEventType](v)
}

func keyEventType(v string) oapi.BrowserCdpKeyEventType {
	return enumOf[oapi.BrowserCdpKeyEventType](v)
}

func touchEventType(v string) oapi.BrowserCdpTouchEventType {
	return enumOf[oapi.BrowserCdpTouchEventType](v)
}

func dragEventType(v string) oapi.BrowserCdpDragEventType {
	return enumOf[oapi.BrowserCdpDragEventType](v)
}

func webLifecycleState(v string) oapi.BrowserCdpWebLifecycleState {
	return enumOf[oapi.BrowserCdpWebLifecycleState](v)
}

func mouseButton(v *string) *oapi.BrowserCdpMouseButton {
	return optionalEnumOf[oapi.BrowserCdpMouseButton](v)
}

func pointerType(v *string) *oapi.BrowserCdpPointerType {
	return optionalEnumOf[oapi.BrowserCdpPointerType](v)
}

func gestureSourceType(v *string) *oapi.BrowserCdpGestureSourceType {
	return optionalEnumOf[oapi.BrowserCdpGestureSourceType](v)
}

func screenshotFormat(v *string) *oapi.BrowserCdpScreenshotFormat {
	return optionalEnumOf[oapi.BrowserCdpScreenshotFormat](v)
}

func snapshotFormat(v *string) *oapi.BrowserCdpSnapshotFormat {
	return optionalEnumOf[oapi.BrowserCdpSnapshotFormat](v)
}

func screencastFormat(v *string) *oapi.BrowserCdpScreencastFormat {
	return optionalEnumOf[oapi.BrowserCdpScreencastFormat](v)
}

func pdfTransferMode(v *string) *oapi.BrowserCdpPdfTransferMode {
	return optionalEnumOf[oapi.BrowserCdpPdfTransferMode](v)
}

func windowState(v *string) *oapi.BrowserCdpWindowState {
	return optionalEnumOf[oapi.BrowserCdpWindowState](v)
}

func transitionType(v *string) *oapi.BrowserCdpTransitionType {
	return optionalEnumOf[oapi.BrowserCdpTransitionType](v)
}

func referrerPolicy(v *string) *oapi.BrowserCdpReferrerPolicy {
	return optionalEnumOf[oapi.BrowserCdpReferrerPolicy](v)
}

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
const maxURLSchemeBytes = 32

func urlScheme(raw string) *string {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		return nil
	}
	scheme := parsed.Scheme
	if len(scheme) > maxURLSchemeBytes {
		scheme = scheme[:maxURLSchemeBytes]
	}
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
		CommandId:          cmd.ID,
		ConnectionId:       cmd.connID(),
		EventType:          mouseEventType(p.Type),
		X:                  p.X,
		Y:                  p.Y,
		Modifiers:          p.Modifiers,
		Button:             mouseButton(p.Button),
		Buttons:            p.Buttons,
		ClickCount:         p.ClickCount,
		DeltaX:             p.DeltaX,
		DeltaY:             p.DeltaY,
		PointerType:        pointerType(p.PointerType),
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		EventType:    keyEventType(p.Type),
		Modifiers:    p.Modifiers,
		TextLength:   runeLen(p.Text),
		NamedKey:     namedKey(p.Key),
		Location:     p.Location,
		AutoRepeat:   p.AutoRepeat,
		IsKeypad:     p.IsKeypad,
		IsSystemKey:  p.IsSystemKey,
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		TextLength:   utf8.RuneCountInString(p.Text),
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
		CommandId:        cmd.ID,
		ConnectionId:     cmd.connID(),
		TextLength:       utf8.RuneCountInString(p.Text),
		SelectionStart:   p.SelectionStart,
		SelectionEnd:     p.SelectionEnd,
		ReplacementStart: p.ReplacementStart,
		ReplacementEnd:   p.ReplacementEnd,
	})
}

type touchPoint struct {
	X                  *float64 `json:"x"`
	Y                  *float64 `json:"y"`
	RadiusX            *float64 `json:"radiusX"`
	RadiusY            *float64 `json:"radiusY"`
	RotationAngle      *float64 `json:"rotationAngle"`
	Force              *float64 `json:"force"`
	TangentialPressure *float64 `json:"tangentialPressure"`
	TiltX              *float64 `json:"tiltX"`
	TiltY              *float64 `json:"tiltY"`
	Twist              *int     `json:"twist"`
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
		CommandId:       cmd.ID,
		ConnectionId:    cmd.connID(),
		EventType:       touchEventType(p.Type),
		TouchPointCount: len(p.TouchPoints),
		Modifiers:       p.Modifiers,
	}
	// A touch dispatch carries its per-point detail inside touchPoints rather
	// than at the top level, so the primary point stands in for the gesture.
	if len(p.TouchPoints) > 0 {
		primary := p.TouchPoints[0]
		data.X, data.Y = primary.X, primary.Y
		data.RadiusX, data.RadiusY = primary.RadiusX, primary.RadiusY
		data.RotationAngle = primary.RotationAngle
		data.Force = primary.Force
		data.TangentialPressure = primary.TangentialPressure
		data.TiltX, data.TiltY = primary.TiltX, primary.TiltY
		data.Twist = primary.Twist
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
		CommandId:          cmd.ID,
		ConnectionId:       cmd.connID(),
		EventType:          dragEventType(p.Type),
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
func mimeCategoriesOf(items []dragDataItem) []oapi.BrowserCdpDragMimeCategory {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		category, _, _ := strings.Cut(item.MimeType, "/")
		category = strings.ToLower(strings.TrimSpace(category))
		if _, ok := mimeCategories[category]; !ok {
			category = "other"
		}
		seen[category] = struct{}{}
	}
	out := make([]oapi.BrowserCdpDragMimeCategory, 0, len(seen))
	for category := range seen {
		out = append(out, oapi.BrowserCdpDragMimeCategory(category))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sanitizeInputCancelDragging(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpInputCancelDraggingCommandData(oapi.BrowserCdpInputCancelDraggingCommandData{
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		EventType:    mouseEventType(p.Type),
		X:            p.X,
		Y:            p.Y,
		Button:       mouseButton(p.Button),
		Modifiers:    p.Modifiers,
		ClickCount:   p.ClickCount,
		DeltaX:       p.DeltaX,
		DeltaY:       p.DeltaY,
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
		CommandId:         cmd.ID,
		ConnectionId:      cmd.connID(),
		X:                 p.X,
		Y:                 p.Y,
		ScaleFactor:       p.ScaleFactor,
		RelativeSpeed:     p.RelativeSpeed,
		GestureSourceType: gestureSourceType(p.GestureSourceType),
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
		CommandId:         cmd.ID,
		ConnectionId:      cmd.connID(),
		X:                 p.X,
		Y:                 p.Y,
		XDistance:         p.XDistance,
		YDistance:         p.YDistance,
		XOverscroll:       p.XOverscroll,
		YOverscroll:       p.YOverscroll,
		PreventFling:      p.PreventFling,
		Speed:             p.Speed,
		GestureSourceType: gestureSourceType(p.GestureSourceType),
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
		CommandId:         cmd.ID,
		ConnectionId:      cmd.connID(),
		X:                 p.X,
		Y:                 p.Y,
		Duration:          p.Duration,
		TapCount:          p.TapCount,
		GestureSourceType: gestureSourceType(p.GestureSourceType),
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
		CommandId:     cmd.ID,
		ConnectionId:  cmd.connID(),
		FileCount:     len(p.Files),
		NodeId:        p.NodeId,
		BackendNodeId: p.BackendNodeId,
		ObjectId:      clipIDPtr(p.ObjectId),
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
		CommandId:     cmd.ID,
		ConnectionId:  cmd.connID(),
		NodeId:        p.NodeId,
		BackendNodeId: p.BackendNodeId,
		ObjectId:      clipIDPtr(p.ObjectId),
	})
}

// rect is DOM.Rect: an offset and a size, none of it sensitive.
type rect struct {
	X      *float64 `json:"x"`
	Y      *float64 `json:"y"`
	Width  *float64 `json:"width"`
	Height *float64 `json:"height"`
}

type domScrollIntoViewIfNeededParams struct {
	domNodeRef
	Rect *rect `json:"rect"`
}

func sanitizeDomScrollIntoViewIfNeeded(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p domScrollIntoViewIfNeededParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	data := oapi.BrowserCdpDomScrollIntoViewIfNeededCommandData{
		SessionId:     cmd.sessionID(),
		CommandId:     cmd.ID,
		ConnectionId:  cmd.connID(),
		NodeId:        p.NodeId,
		BackendNodeId: p.BackendNodeId,
		ObjectId:      clipIDPtr(p.ObjectId),
	}
	if p.Rect != nil {
		data.RectX, data.RectY = p.Rect.X, p.Rect.Y
		data.RectWidth, data.RectHeight = p.Rect.Width, p.Rect.Height
	}
	return out, out.FromBrowserCdpDomScrollIntoViewIfNeededCommandData(data)
}

// ---- Page ----

func sanitizePageBringToFront(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpPageBringToFrontCommandData(oapi.BrowserCdpPageBringToFrontCommandData{
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
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
		CommandId:             cmd.ID,
		ConnectionId:          cmd.connID(),
		Format:                screenshotFormat(p.Format),
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		Format:       snapshotFormat(p.Format),
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
		CommandId:        cmd.ID,
		ConnectionId:     cmd.connID(),
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
		CommandId:       cmd.ID,
		ConnectionId:    cmd.connID(),
		UrlScheme:       urlScheme(p.Url),
		TransitionType:  transitionType(p.TransitionType),
		ReferrerPresent: present(p.Referrer),
		ReferrerPolicy:  referrerPolicy(p.ReferrerPolicy),
		FrameId:         clipIDPtr(p.FrameId),
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		EntryId:      p.EntryId,
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		IgnoreCache:  p.IgnoreCache,
		ScriptLength: runeLen(p.ScriptToEvaluateOnLoad),
		LoaderId:     clipIDPtr(p.LoaderId),
	})
}

type pagePrintToPDFParams struct {
	Landscape               *bool    `json:"landscape"`
	DisplayHeaderFooter     *bool    `json:"displayHeaderFooter"`
	PrintBackground         *bool    `json:"printBackground"`
	Scale                   *float64 `json:"scale"`
	PaperWidth              *float64 `json:"paperWidth"`
	PaperHeight             *float64 `json:"paperHeight"`
	PageRanges              *string  `json:"pageRanges"`
	HeaderTemplate          *string  `json:"headerTemplate"`
	FooterTemplate          *string  `json:"footerTemplate"`
	MarginTop               *float64 `json:"marginTop"`
	MarginBottom            *float64 `json:"marginBottom"`
	MarginLeft              *float64 `json:"marginLeft"`
	MarginRight             *float64 `json:"marginRight"`
	PreferCSSPageSize       *bool    `json:"preferCSSPageSize"`
	TransferMode            *string  `json:"transferMode"`
	GenerateTaggedPDF       *bool    `json:"generateTaggedPDF"`
	GenerateDocumentOutline *bool    `json:"generateDocumentOutline"`
}

func sanitizePagePrintToPDF(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p pagePrintToPDFParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpPagePrintToPdfCommandData(oapi.BrowserCdpPagePrintToPdfCommandData{
		SessionId:               cmd.sessionID(),
		CommandId:               cmd.ID,
		ConnectionId:            cmd.connID(),
		Landscape:               p.Landscape,
		Scale:                   p.Scale,
		PaperWidth:              p.PaperWidth,
		PaperHeight:             p.PaperHeight,
		DisplayHeaderFooter:     p.DisplayHeaderFooter,
		PrintBackground:         p.PrintBackground,
		MarginTop:               p.MarginTop,
		MarginBottom:            p.MarginBottom,
		MarginLeft:              p.MarginLeft,
		MarginRight:             p.MarginRight,
		PreferCssPageSize:       p.PreferCSSPageSize,
		TransferMode:            pdfTransferMode(p.TransferMode),
		GenerateTaggedPdf:       p.GenerateTaggedPDF,
		GenerateDocumentOutline: p.GenerateDocumentOutline,
		PageRangesPresent:       present(p.PageRanges),
		HeaderTemplatePresent:   present(p.HeaderTemplate),
		FooterTemplatePresent:   present(p.FooterTemplate),
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
		CommandId:     cmd.ID,
		ConnectionId:  cmd.connID(),
		Format:        screencastFormat(p.Format),
		Quality:       p.Quality,
		MaxWidth:      p.MaxWidth,
		MaxHeight:     p.MaxHeight,
		EveryNthFrame: p.EveryNthFrame,
	})
}

func sanitizePageStopScreencast(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpPageStopScreencastCommandData(oapi.BrowserCdpPageStopScreencastCommandData{
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
	})
}

func sanitizePageStopLoading(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpPageStopLoadingCommandData(oapi.BrowserCdpPageStopLoadingCommandData{
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
	})
}

func sanitizePageClose(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpPageCloseCommandData(oapi.BrowserCdpPageCloseCommandData{
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		State:        webLifecycleState(p.State),
	})
}

// ---- Target ----

type targetIdParams struct {
	TargetId string `json:"targetId"`
}

// Target.openDevTools is the only one of the three that takes a panel.
type targetOpenDevToolsParams struct {
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		TargetId:     clipID(p.TargetId),
	})
}

func sanitizeTargetCloseTarget(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p targetIdParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpTargetCloseTargetCommandData(oapi.BrowserCdpTargetCloseTargetCommandData{
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		TargetId:     clipID(p.TargetId),
	})
}

func sanitizeTargetOpenDevTools(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p targetOpenDevToolsParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	data := oapi.BrowserCdpTargetOpenDevToolsCommandData{
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		TargetId:     clipID(p.TargetId),
	}
	if p.PanelId != "" {
		clipped := clipID(p.PanelId)
		data.PanelId = &clipped
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
	Focus                   *bool   `json:"focus"`
}

func sanitizeTargetCreateTarget(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p targetCreateTargetParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	return out, out.FromBrowserCdpTargetCreateTargetCommandData(oapi.BrowserCdpTargetCreateTargetCommandData{
		SessionId:               cmd.sessionID(),
		CommandId:               cmd.ID,
		ConnectionId:            cmd.connID(),
		UrlScheme:               urlScheme(p.Url),
		Left:                    p.Left,
		Top:                     p.Top,
		Width:                   p.Width,
		Height:                  p.Height,
		WindowState:             windowState(p.WindowState),
		BrowserContextId:        clipIDPtr(p.BrowserContextId),
		NewWindow:               p.NewWindow,
		Background:              p.Background,
		ForTab:                  p.ForTab,
		Hidden:                  p.Hidden,
		EnableBeginFrameControl: p.EnableBeginFrameControl,
		Focus:                   p.Focus,
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
		CommandId:                         cmd.ID,
		ConnectionId:                      cmd.connID(),
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
		CommandId:        cmd.ID,
		ConnectionId:     cmd.connID(),
		BrowserContextId: clipID(p.BrowserContextId),
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
		CommandId:        cmd.ID,
		ConnectionId:     cmd.connID(),
		DownloadGuid:     clipID(p.Guid),
		BrowserContextId: clipIDPtr(p.BrowserContextId),
	})
}

func sanitizeBrowserClose(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var out oapi.BrowserCdpCommandEventData
	return out, out.FromBrowserCdpBrowserCloseCommandData(oapi.BrowserCdpBrowserCloseCommandData{
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		WindowId:     p.WindowId,
		Left:         p.Bounds.Left,
		Top:          p.Bounds.Top,
		Width:        p.Bounds.Width,
		Height:       p.Bounds.Height,
		WindowState:  windowState(p.Bounds.WindowState),
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
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		WindowId:     p.WindowId,
		Width:        p.Width,
		Height:       p.Height,
	})
}

// ---- Autofill ----

// autofillAddress counts the fields an address carries. Their names are
// caller-supplied strings and their values are the address itself, so neither
// is decoded.
type autofillAddress struct {
	Fields []struct{} `json:"fields"`
}

type autofillTriggerParams struct {
	FieldId int              `json:"fieldId"`
	FrameId *string          `json:"frameId"`
	Card    json.RawMessage  `json:"card"`
	Address *autofillAddress `json:"address"`
}

func sanitizeAutofillTrigger(cmd cdpCommand) (oapi.BrowserCdpCommandEventData, error) {
	var p autofillTriggerParams
	var out oapi.BrowserCdpCommandEventData
	if err := decodeParams(cmd.Params, &p); err != nil {
		return out, err
	}
	data := oapi.BrowserCdpAutofillTriggerCommandData{
		SessionId:    cmd.sessionID(),
		CommandId:    cmd.ID,
		ConnectionId: cmd.connID(),
		FieldId:      p.FieldId,
		FrameId:      clipIDPtr(p.FrameId),
	}
	// The card number and the address lines are the whole payload, so only
	// which of the two was filled survives.
	switch {
	case len(p.Card) > 0 && string(p.Card) != "null":
		mode := oapi.Card
		data.Mode = &mode
	case p.Address != nil:
		mode := oapi.Address
		data.Mode = &mode
		data.AddressFieldCount = count(p.Address.Fields)
	}
	return out, out.FromBrowserCdpAutofillTriggerCommandData(data)
}
