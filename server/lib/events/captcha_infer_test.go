package events

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oapi "github.com/kernel/kernel-images/server/lib/oapi"
)

func TestInferCaptchaChallengeResult(t *testing.T) {
	solveResult := func(data string) Event {
		return Event{Ts: 42, Type: "captcha_solve_result", Category: Captcha, Source: oapi.BrowserEventSource{Kind: oapi.Extension}, Data: json.RawMessage(data)}
	}

	t.Run("turnstile success becomes an inferred solved challenge", func(t *testing.T) {
		got, ok := InferCaptchaChallengeResult(solveResult(`{"captcha_type":"turnstile","status":"success","duration_ms":1234.5,"task_id":"t1","website_host":"example.com","website_path":"/login"}`))
		require.True(t, ok)
		assert.Equal(t, int64(42), got.Ts)
		assert.Equal(t, "captcha_challenge_result", got.Type)
		assert.Equal(t, Captcha, got.Category)
		assert.Equal(t, oapi.KernelApi, got.Source.Kind)
		assert.JSONEq(t, `{"captcha_type":"turnstile","status":"solved","duration_ms":1234.5,"inferred":true,"task_id":"t1","website_host":"example.com","website_path":"/login"}`, string(got.Data))
	})

	for _, tc := range []struct{ task, challenge string }{
		{"failure", "failure"},
		{"timeout", "timeout"},
		{"abandoned", "abandoned"},
	} {
		t.Run("task "+tc.task+" maps to challenge "+tc.challenge, func(t *testing.T) {
			got, ok := InferCaptchaChallengeResult(solveResult(`{"captcha_type":"geetest","status":"` + tc.task + `","duration_ms":1,"task_id":"t2","error_code":"ERROR_CAPTCHA_UNSOLVABLE"}`))
			require.True(t, ok)
			var data oapi.BrowserCaptchaChallengeResultEventData
			require.NoError(t, json.Unmarshal(got.Data, &data))
			assert.Equal(t, oapi.BrowserCaptchaChallengeResultEventDataStatus(tc.challenge), data.Status)
			assert.Nil(t, data.ChallengeId)
		})
	}

	for _, tc := range []struct {
		name string
		ev   Event
	}{
		{"observed type", solveResult(`{"captcha_type":"recaptcha_v2","status":"success","duration_ms":1,"task_id":"t3"}`)},
		{"image-grid round", solveResult(`{"captcha_type":"other","status":"success","duration_ms":1,"task_id":"t4"}`)},
		{"task that already belongs to a challenge", solveResult(`{"captcha_type":"recaptcha_v3","status":"success","duration_ms":1,"task_id":"t5","challenge_id":"c1"}`)},
		{"unknown status", solveResult(`{"captcha_type":"turnstile","status":"processing","duration_ms":1,"task_id":"t6"}`)},
		{"malformed data", solveResult(`not json`)},
		{"solve started", Event{Type: "captcha_solve_started", Category: Captcha, Data: json.RawMessage(`{"captcha_type":"turnstile","task_id":"t7"}`)}},
	} {
		t.Run(tc.name+" infers nothing", func(t *testing.T) {
			_, ok := InferCaptchaChallengeResult(tc.ev)
			assert.False(t, ok)
		})
	}
}
