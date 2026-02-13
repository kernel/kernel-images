package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onkernel/kernel-images/server/lib/devtoolsproxy"
	"github.com/onkernel/kernel-images/server/lib/logger"
	"github.com/onkernel/kernel-images/server/lib/nekoclient"
	oapi "github.com/onkernel/kernel-images/server/lib/oapi"
	"github.com/onkernel/kernel-images/server/lib/policy"
	"github.com/onkernel/kernel-images/server/lib/recorder"
	"github.com/onkernel/kernel-images/server/lib/scaletozero"
)

type ApiService struct {
	// defaultRecorderID is used whenever the caller doesn't specify an explicit ID.
	defaultRecorderID string

	recordManager recorder.RecordManager
	factory       recorder.FFmpegRecorderFactory
	// Filesystem watch management
	watchMu sync.RWMutex
	watches map[string]*fsWatch

	// Process management
	procMu sync.RWMutex
	procs  map[string]*processHandle

	// Neko authenticated client
	nekoAuthClient *nekoclient.AuthClient

	// DevTools upstream manager (Chromium supervisord log tailer)
	upstreamMgr *devtoolsproxy.UpstreamManager
	stz         scaletozero.Controller

	// inputMu serializes input-related operations (mouse, keyboard, screenshot)
	inputMu sync.Mutex

	// playwrightMu serializes Playwright code execution (only one execution at a time)
	playwrightMu sync.Mutex

	// playwrightDaemonStarting is an atomic flag to prevent concurrent daemon starts
	playwrightDaemonStarting int32

	// playwrightDaemonCmd holds the daemon process for cleanup
	playwrightDaemonCmd *exec.Cmd

	// policy management
	policy *policy.Policy

	// chromiumConfigMu serializes mutable Chromium runtime config under /chromium.
	chromiumConfigMu sync.Mutex
	// Runtime Chromium flags overlay path.
	chromiumFlagsPath string
	// PAC script path managed by /chromium/proxy/pac endpoints.
	chromiumPACPath string
	// PAC script URL served by this API and exposed in PAC status responses.
	chromiumPACURL string
	// PAC OS-level apply state path used for GET status reporting.
	chromiumPACStatePath string
}

var _ oapi.StrictServerInterface = (*ApiService)(nil)

func New(recordManager recorder.RecordManager, factory recorder.FFmpegRecorderFactory, upstreamMgr *devtoolsproxy.UpstreamManager, stz scaletozero.Controller, nekoAuthClient *nekoclient.AuthClient) (*ApiService, error) {
	switch {
	case recordManager == nil:
		return nil, fmt.Errorf("recordManager cannot be nil")
	case factory == nil:
		return nil, fmt.Errorf("factory cannot be nil")
	case upstreamMgr == nil:
		return nil, fmt.Errorf("upstreamMgr cannot be nil")
	case nekoAuthClient == nil:
		return nil, fmt.Errorf("nekoAuthClient cannot be nil")
	}

	apiPort := 10001
	if rawPort := strings.TrimSpace(os.Getenv("PORT")); rawPort != "" {
		if parsed, err := strconv.Atoi(rawPort); err == nil && parsed > 0 {
			apiPort = parsed
		}
	}

	return &ApiService{
		recordManager:        recordManager,
		factory:              factory,
		defaultRecorderID:    "default",
		watches:              make(map[string]*fsWatch),
		procs:                make(map[string]*processHandle),
		upstreamMgr:          upstreamMgr,
		stz:                  stz,
		nekoAuthClient:       nekoAuthClient,
		policy:               &policy.Policy{},
		chromiumFlagsPath:    "/chromium/flags",
		chromiumPACPath:      "/chromium/proxy.pac",
		chromiumPACURL:       fmt.Sprintf("http://127.0.0.1:%d/chromium/proxy/pac/script", apiPort),
		chromiumPACStatePath: "/chromium/pac-state.json",
	}, nil
}

func (s *ApiService) StartRecording(ctx context.Context, req oapi.StartRecordingRequestObject) (oapi.StartRecordingResponseObject, error) {
	log := logger.FromContext(ctx)

	var params recorder.FFmpegRecordingParams
	if req.Body != nil {
		params.FrameRate = req.Body.Framerate
		params.MaxSizeInMB = req.Body.MaxFileSizeInMB
		params.MaxDurationInSeconds = req.Body.MaxDurationInSeconds
	}

	// Determine recorder ID (use default if none provided)
	recorderID := s.defaultRecorderID
	if req.Body != nil && req.Body.Id != nil && *req.Body.Id != "" {
		recorderID = *req.Body.Id
	}

	// Create, register, and start a new recorder
	rec, err := s.factory(recorderID, params)
	if err != nil {
		log.Error("failed to create recorder", "err", err, "recorder_id", recorderID)
		return oapi.StartRecording500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to create recording"}}, nil
	}
	if err := s.recordManager.RegisterRecorder(ctx, rec); err != nil {
		if rec, exists := s.recordManager.GetRecorder(recorderID); exists {
			if rec.IsRecording(ctx) {
				log.Error("attempted to start recording while one is already active", "recorder_id", recorderID)
				return oapi.StartRecording409JSONResponse{ConflictErrorJSONResponse: oapi.ConflictErrorJSONResponse{Message: "recording already in progress"}}, nil
			} else {
				log.Error("attempted to restart recording", "recorder_id", recorderID)
				return oapi.StartRecording400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "recording already completed"}}, nil
			}
		}
		log.Error("failed to register recorder", "err", err, "recorder_id", recorderID)
		return oapi.StartRecording500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to register recording"}}, nil
	}

	if err := rec.Start(ctx); err != nil {
		log.Error("failed to start recording", "err", err, "recorder_id", recorderID)
		// ensure the recorder is deregistered
		defer s.recordManager.DeregisterRecorder(ctx, rec)
		return oapi.StartRecording500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to start recording"}}, nil
	}

	return oapi.StartRecording201Response{}, nil
}

func (s *ApiService) StopRecording(ctx context.Context, req oapi.StopRecordingRequestObject) (oapi.StopRecordingResponseObject, error) {
	log := logger.FromContext(ctx)

	// Determine recorder ID
	recorderID := s.defaultRecorderID
	if req.Body != nil && req.Body.Id != nil && *req.Body.Id != "" {
		recorderID = *req.Body.Id
	}

	rec, exists := s.recordManager.GetRecorder(recorderID)
	if !exists {
		log.Error("attempted to stop recording when none is active", "recorder_id", recorderID)
		return oapi.StopRecording400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "no active recording to stop"}}, nil
	}
	// Always call Stop() even if IsRecording() is false. Recordings that exit naturally
	// (max duration, max file size, etc.) finalize automatically, but Stop() is still
	// needed to update scale-to-zero state and ensure clean shutdown.

	// Check if force stop is requested
	forceStop := false
	if req.Body != nil && req.Body.ForceStop != nil {
		forceStop = *req.Body.ForceStop
	}

	var err error
	if forceStop {
		log.Info("force stopping recording", "recorder_id", recorderID)
		err = rec.ForceStop(ctx)
	} else {
		log.Info("gracefully stopping recording", "recorder_id", recorderID)
		err = rec.Stop(ctx)
	}

	if err != nil {
		log.Error("error occurred while stopping recording", "err", err, "force", forceStop, "recorder_id", recorderID)
	}

	return oapi.StopRecording200Response{}, nil
}

const (
	minRecordingSizeInBytes = 100
)

func (s *ApiService) DownloadRecording(ctx context.Context, req oapi.DownloadRecordingRequestObject) (oapi.DownloadRecordingResponseObject, error) {
	log := logger.FromContext(ctx)

	// Determine recorder ID
	recorderID := s.defaultRecorderID
	if req.Params.Id != nil && *req.Params.Id != "" {
		recorderID = *req.Params.Id
	}

	// Get the recorder to access its output path
	rec, exists := s.recordManager.GetRecorder(recorderID)
	if !exists {
		log.Error("attempted to download non-existent recording", "recorder_id", recorderID)
		return oapi.DownloadRecording404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "no recording found"}}, nil
	}
	if rec.IsDeleted(ctx) {
		log.Error("attempted to download deleted recording", "recorder_id", recorderID)
		return oapi.DownloadRecording400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "requested recording has been deleted"}}, nil
	}

	out, meta, err := rec.Recording(ctx)
	if err != nil {
		if errors.Is(err, recorder.ErrRecordingFinalizing) {
			// Wait for finalization to complete instead of asking client to retry
			log.Info("waiting for recording finalization", "recorder_id", recorderID)
			ffmpegRec, ok := rec.(*recorder.FFmpegRecorder)
			if !ok {
				log.Error("failed to cast recorder to FFmpegRecorder", "recorder_id", recorderID)
				return oapi.DownloadRecording500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "internal error"}}, nil
			}
			// WaitForFinalization blocks until finalization completes and returns the result
			if finalizeErr := ffmpegRec.WaitForFinalization(ctx); finalizeErr != nil {
				log.Error("finalization failed", "err", finalizeErr, "recorder_id", recorderID)
				return oapi.DownloadRecording500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to finalize recording"}}, nil
			}
			// Finalization complete, retry getting the recording
			out, meta, err = rec.Recording(ctx)
			if err != nil {
				log.Error("failed to get recording after finalization", "err", err, "recorder_id", recorderID)
				return oapi.DownloadRecording500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to get recording"}}, nil
			}
		} else {
			log.Error("failed to get recording", "err", err, "recorder_id", recorderID)
			return oapi.DownloadRecording500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to get recording"}}, nil
		}
	}

	// short-circuit if the recording is still in progress and the file is arbitrary small
	if rec.IsRecording(ctx) && meta.Size <= minRecordingSizeInBytes {
		out.Close() // Close the file handle to prevent descriptor leak
		return oapi.DownloadRecording202Response{
			Headers: oapi.DownloadRecording202ResponseHeaders{
				RetryAfter: 300,
			},
		}, nil
	}

	log.Info("serving recording file for download", "size", meta.Size, "recorder_id", recorderID)
	return oapi.DownloadRecording200Videomp4Response{
		Body: out,
		Headers: oapi.DownloadRecording200ResponseHeaders{
			XRecordingStartedAt:  meta.StartTime.Format(time.RFC3339),
			XRecordingFinishedAt: meta.EndTime.Format(time.RFC3339),
		},
		ContentLength: meta.Size,
	}, nil
}

func (s *ApiService) DeleteRecording(ctx context.Context, req oapi.DeleteRecordingRequestObject) (oapi.DeleteRecordingResponseObject, error) {
	log := logger.FromContext(ctx)

	recorderID := s.defaultRecorderID
	if req.Body != nil && req.Body.Id != nil && *req.Body.Id != "" {
		recorderID = *req.Body.Id
	}
	rec, exists := s.recordManager.GetRecorder(recorderID)
	if !exists {
		log.Error("attempted to delete non-existent recording", "recorder_id", recorderID)
		return oapi.DeleteRecording404JSONResponse{NotFoundErrorJSONResponse: oapi.NotFoundErrorJSONResponse{Message: "no recording found"}}, nil
	}

	if rec.IsRecording(ctx) {
		log.Error("attempted to delete recording while still in progress", "recorder_id", recorderID)
		return oapi.DeleteRecording400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "recording must be stopped first"}}, nil
	}

	if err := rec.Delete(ctx); err != nil {
		if errors.Is(err, recorder.ErrRecordingFinalizing) {
			log.Info("recording is being finalized, client should retry", "recorder_id", recorderID)
			return oapi.DeleteRecording409JSONResponse{ConflictErrorJSONResponse: oapi.ConflictErrorJSONResponse{Message: "recording is being finalized, please retry in a few seconds"}}, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			log.Error("failed to delete recording", "err", err, "recorder_id", recorderID)
			return oapi.DeleteRecording500JSONResponse{InternalErrorJSONResponse: oapi.InternalErrorJSONResponse{Message: "failed to delete recording"}}, nil
		}
	}

	log.Info("recording deleted", "recorder_id", recorderID)
	return oapi.DeleteRecording200Response{}, nil
}

// ListRecorders returns a list of all registered recorders and whether each one is currently recording.
func (s *ApiService) ListRecorders(ctx context.Context, _ oapi.ListRecordersRequestObject) (oapi.ListRecordersResponseObject, error) {
	infos := []oapi.RecorderInfo{}

	timeOrNil := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return &t
	}

	recs := s.recordManager.ListActiveRecorders(ctx)
	for _, r := range recs {
		m := r.Metadata()
		infos = append(infos, oapi.RecorderInfo{
			Id:          r.ID(),
			IsRecording: r.IsRecording(ctx),
			StartedAt:   timeOrNil(m.StartTime),
			FinishedAt:  timeOrNil(m.EndTime),
		})
	}
	return oapi.ListRecorders200JSONResponse(infos), nil
}

func (s *ApiService) Shutdown(ctx context.Context) error {
	return s.recordManager.StopAll(ctx)
}
