package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/kernel/kernel-images/server/lib/oapi"
)

const maxVaultFillRequestBytes = 1024 * 1024

// This protocol is separate from code execution: no generated code, raw errors,
// stacks or caller-provided strings are returned or attached to telemetry.
type vaultDaemonRequest struct {
	ID      string                 `json:"id"`
	Method  string                 `json:"method"`
	Request *oapi.VaultFillRequest `json:"request,omitempty"`
}

type vaultDaemonResponse struct {
	ID      string          `json:"id"`
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
}

func callVaultDaemon(ctx context.Context, socket string, request vaultDaemonRequest, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, errors.New("executor_unavailable")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, errors.New("executor_unavailable")
	}
	request.ID = uuid.NewString()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, errors.New("executor_unknown")
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 64*1024)).ReadBytes('\n')
	if err != nil {
		return nil, errors.New("executor_unknown")
	}
	var response vaultDaemonResponse
	if json.Unmarshal(line, &response) != nil || response.ID != request.ID || !response.Success {
		return nil, errors.New("executor_unknown")
	}
	return response.Result, nil
}

func (s *ApiService) GetVaultFillCapabilities(ctx context.Context, _ oapi.GetVaultFillCapabilitiesRequestObject) (oapi.GetVaultFillCapabilitiesResponseObject, error) {
	unavailable := oapi.GetVaultFillCapabilities503JSONResponse{Message: "executor_unavailable"}
	if err := s.ensurePlaywrightDaemon(ctx); err != nil {
		return unavailable, nil
	}
	result, err := callVaultDaemon(ctx, playwrightDaemonSocket, vaultDaemonRequest{Method: "vault_fill_capabilities"}, 2*time.Second)
	var capabilities oapi.VaultFillCapabilities
	if err != nil || json.Unmarshal(result, &capabilities) != nil || capabilities.Version != oapi.N1 {
		return unavailable, nil
	}
	return oapi.GetVaultFillCapabilities200JSONResponse{Version: oapi.N1}, nil
}

func validVaultFillRequest(request *oapi.VaultFillRequest) bool {
	if request == nil || len(request.Bindings) == 0 || len(request.Bindings) > 100 {
		return false
	}
	if request.TimeoutMs != nil && (*request.TimeoutMs < 1 || *request.TimeoutMs > 30000) {
		return false
	}
	if request.PageUrl != nil && len(*request.PageUrl) > 8192 {
		return false
	}
	for _, binding := range request.Bindings {
		if len(binding.Selector) == 0 || len(binding.Selector) > 4096 || binding.Value == nil || len(*binding.Value) > 65536 || !binding.Type.Valid() {
			return false
		}
	}
	return true
}

func unknownVaultFillResult(count int) oapi.VaultFillResult {
	fields := make([]oapi.VaultFillFieldResult, count)
	for i := range fields {
		fields[i] = oapi.VaultFillFieldResult{Index: i, Status: oapi.VaultFillFieldResultStatusUnknown}
	}
	return oapi.VaultFillResult{Status: oapi.VaultFillResultStatusUnknown, Fields: fields}
}

func parseVaultFillResult(data []byte, count int) oapi.VaultFillResult {
	var result oapi.VaultFillResult
	if json.Unmarshal(data, &result) != nil || !result.Status.Valid() || len(result.Fields) != count {
		return unknownVaultFillResult(count)
	}
	for i, field := range result.Fields {
		if field.Index != i || !field.Status.Valid() {
			return unknownVaultFillResult(count)
		}
	}
	return result
}

func (s *ApiService) FillVault(ctx context.Context, request oapi.FillVaultRequestObject) (oapi.FillVaultResponseObject, error) {
	if !validVaultFillRequest(request.Body) {
		return oapi.FillVault400JSONResponse{BadRequestErrorJSONResponse: oapi.BadRequestErrorJSONResponse{Message: "invalid_request"}}, nil
	}
	// Do not queue credential writes behind arbitrary user code, or interleave
	// them with another fill. A busy executor has not attempted any writes.
	if !s.playwrightMu.TryLock() {
		return oapi.FillVault503JSONResponse{Message: "executor_unavailable"}, nil
	}
	defer s.playwrightMu.Unlock()
	if err := s.ensurePlaywrightDaemon(ctx); err != nil {
		return oapi.FillVault503JSONResponse{Message: "executor_unavailable"}, nil
	}
	timeout := 10000
	if request.Body.TimeoutMs != nil {
		timeout = *request.Body.TimeoutMs
	}
	result, err := callVaultDaemon(ctx, playwrightDaemonSocket, vaultDaemonRequest{
		Method: "vault_fill", Request: request.Body,
	}, time.Duration(timeout)*time.Millisecond+2*time.Second)
	if err != nil {
		return oapi.FillVault200JSONResponse(unknownVaultFillResult(len(request.Body.Bindings))), nil
	}
	return oapi.FillVault200JSONResponse(parseVaultFillResult(result, len(request.Body.Bindings))), nil
}
