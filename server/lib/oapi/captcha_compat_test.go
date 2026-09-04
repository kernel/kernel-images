package oapi

import "testing"

func TestCaptchaGeneratedNamesRemainCompatible(t *testing.T) {
	var captchaType BrowserCaptchaSolveResultEventDataCaptchaType = BrowserCaptchaSolveResultEventDataCaptchaTypeHcaptcha
	var sharedType BrowserCaptchaType = captchaType
	if sharedType != captchaType {
		t.Fatalf("shared captcha type = %q, want %q", sharedType, captchaType)
	}

	statuses := []BrowserCaptchaSolveResultEventDataStatus{
		Success,
		Failure,
		Timeout,
		Abandoned,
	}
	for _, status := range statuses {
		if !status.Valid() {
			t.Fatalf("solve status %q is invalid", status)
		}
	}
	for _, status := range []BrowserCaptchaChallengeResultEventDataStatus{
		ChallengeSolved,
		ChallengeFailure,
		ChallengeTimeout,
		ChallengeAbandoned,
	} {
		if !status.Valid() {
			t.Fatalf("challenge status %q is invalid", status)
		}
	}

	started := BrowserCaptchaSolveStartedEvent{
		Data: BrowserCaptchaSolveStartedEventData{CaptchaType: captchaType},
	}
	challenge := BrowserCaptchaChallengeResultEvent{
		Data: BrowserCaptchaChallengeResultEventData{
			CaptchaType: captchaType,
			ChallengeId: "challenge-id",
			DurationMs:  1,
			Status:      ChallengeFailure,
		},
	}
	if started.Data.CaptchaType != challenge.Data.CaptchaType {
		t.Fatal("captcha event types are inconsistent")
	}
}
