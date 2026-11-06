package nekoclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	nekooapi "github.com/m1k1o/neko/server/lib/oapi"
)

func TestScreenConfigurationChangeStatus(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		wantErr      bool
		wantRejected bool
	}{
		{name: "ok", status: http.StatusOK},
		{name: "unprocessable", status: http.StatusUnprocessableEntity, wantErr: true, wantRejected: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/login":
					_, _ = w.Write([]byte(`{"token":"t"}`))
				case "/api/room/screen":
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			client, err := NewAuthClient(srv.URL, "admin", "admin")
			if err != nil {
				t.Fatal(err)
			}
			width, height, rate := 1008, 384, 60
			err = client.ScreenConfigurationChange(context.Background(), nekooapi.ScreenConfiguration{Width: &width, Height: &height, Rate: &rate})

			if (err != nil) != tc.wantErr {
				t.Fatalf("got error %v, want error %v", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrScreenConfigurationRejected); got != tc.wantRejected {
				t.Fatalf("errors.Is(err, ErrScreenConfigurationRejected) = %v, want %v (err: %v)", got, tc.wantRejected, err)
			}
		})
	}
}
