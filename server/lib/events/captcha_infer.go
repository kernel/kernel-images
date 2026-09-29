package events

import (
	"encoding/json"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
	"github.com/samber/lo"
)

// inferredChallengeTypes are the captcha types no in-VM producer observes as a
// widget, so their challenge outcome can only come from the solver task.
// recaptcha_v2, hcaptcha and press_and_hold report observed results. other is
// left out because it covers single image-grid rounds, where one task is not
// one challenge.
var inferredChallengeTypes = map[oapi.BrowserCaptchaType]struct{}{
	oapi.BrowserCaptchaSolveResultEventDataCaptchaTypeTurnstile:   {},
	oapi.BrowserCaptchaSolveResultEventDataCaptchaTypeGeetest:     {},
	oapi.BrowserCaptchaSolveResultEventDataCaptchaTypeRecaptchaV3: {},
}

var inferredChallengeStatus = map[oapi.BrowserCaptchaSolveResultEventDataStatus]oapi.BrowserCaptchaChallengeResultEventDataStatus{
	oapi.Success:   oapi.ChallengeSolved,
	oapi.Failure:   oapi.ChallengeFailure,
	oapi.Timeout:   oapi.ChallengeTimeout,
	oapi.Abandoned: oapi.ChallengeAbandoned,
}

// InferCaptchaChallengeResult derives a captcha_challenge_result from a
// terminal solver task whose captcha type has no observed widget. ok is false
// for every other event, including a task result that carries a challenge_id,
// because the producer that assigned it reports that challenge itself.
func InferCaptchaChallengeResult(ev Event) (Event, bool) {
	if ev.Type != string(oapi.CaptchaSolveResult) {
		return Event{}, false
	}
	var task oapi.BrowserCaptchaSolveResultEventData
	if err := json.Unmarshal(ev.Data, &task); err != nil {
		return Event{}, false
	}
	if _, ok := inferredChallengeTypes[task.CaptchaType]; !ok || lo.FromPtr(task.ChallengeId) != "" {
		return Event{}, false
	}
	status, ok := inferredChallengeStatus[task.Status]
	if !ok {
		return Event{}, false
	}

	data, err := json.Marshal(oapi.BrowserCaptchaChallengeResultEventData{
		CaptchaType: task.CaptchaType,
		Status:      status,
		DurationMs:  task.DurationMs,
		Inferred:    lo.ToPtr(true),
		TaskId:      task.TaskId,
		WebsiteHost: task.WebsiteHost,
		WebsitePath: task.WebsitePath,
	})
	if err != nil {
		return Event{}, false
	}
	return Event{
		Ts:       ev.Ts,
		Type:     string(oapi.CaptchaChallengeResult),
		Category: Captcha,
		Source:   oapi.BrowserEventSource{Kind: oapi.KernelApi},
		Data:     data,
	}, true
}
