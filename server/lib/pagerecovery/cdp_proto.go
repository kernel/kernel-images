package pagerecovery

// CDP payloads this package reads, holding only the fields it acts on. The
// wider PDL-faithful types live in cdpmonitor, which reports on every field;
// here an unread field would be a field nothing tests.

type cdpHeaderEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cdpFetchRequest struct {
	URL    string `json:"url"`
	Method string `json:"method"`
}

// cdpFetchRequestPaused mirrors the response-stage form of the notification.
// ResponseStatusCode and ResponseErrorReason are mutually exclusive: a request
// that died has no status, and one that answered has no error reason.
type cdpFetchRequestPaused struct {
	RequestID           string           `json:"requestId"`
	Request             cdpFetchRequest  `json:"request"`
	FrameID             string           `json:"frameId"`
	ResourceType        string           `json:"resourceType"`
	ResponseStatusCode  int              `json:"responseStatusCode"`
	ResponseErrorReason string           `json:"responseErrorReason"`
	ResponseHeaders     []cdpHeaderEntry `json:"responseHeaders"`
}

type cdpPageGetFrameTreeResult struct {
	FrameTree struct {
		Frame struct {
			ID string `json:"id"`
		} `json:"frame"`
	} `json:"frameTree"`
}
