package migration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

// TokenProvider returns the shared secret this node expects on migration
// endpoints; empty disables the endpoints entirely (fail closed).
type TokenProvider func() string

// Handler is the migration HTTP surface. The cmd/http-server wiring adapts
// it to its router and provides the token provider.
type Handler struct {
	svc   *Service
	token TokenProvider
}

// NewHandler builds the migration HTTP handler.
func NewHandler(svc *Service, token TokenProvider) *Handler {
	return &Handler{svc: svc, token: token}
}

func (h *Handler) authorized(r *http.Request) bool {
	t := h.token()
	if t == "" {
		// Fail closed: no configured token ⇒ migration disabled.
		return false
	}
	return r.Header.Get("X-Migration-Token") == t
}

func respondJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func respondErr(w http.ResponseWriter, code int, err error) {
	respondJSON(w, code, map[string]string{"error": err.Error()})
}

// bytesReceivedHeader reports how many stream bytes were consumed, so the
// source can render progress.
func bytesReceivedHeader(w http.ResponseWriter, n int64) {
	w.Header().Set("X-Bytes-Received", strconv.FormatInt(n, 10))
}

// InfoHandler serves this node's fingerprint (GET /migration/info).
func (h *Handler) InfoHandler(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.svc.Info())
}

// PrepareHandler (peer) registers an incoming VM (POST /migration/prepare).
func (h *Handler) PrepareHandler(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		respondErr(w, http.StatusUnauthorized, ErrTokenRequired)
		return
	}
	var payload PreparePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		respondErr(w, http.StatusBadRequest, err)
		return
	}
	var cfgJSON []byte
	if payload.SourceConfig != "" {
		b, err := base64.StdEncoding.DecodeString(payload.SourceConfig)
		if err != nil {
			respondErr(w, http.StatusBadRequest, err)
			return
		}
		cfgJSON = b
	}
	if err := h.svc.Prepare(payload.VM, cfgJSON); err != nil {
		respondErr(w, http.StatusConflict, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "prepared"})
}

// DiskStreamHandler (peer) receives a zfs send stream (PUT /migration/{id}/disk).
func (h *Handler) DiskStreamHandler(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		respondErr(w, http.StatusUnauthorized, ErrTokenRequired)
		return
	}
	vmID := pathID(r)
	toSnap := r.Header.Get("X-To-Snap")
	fromSnap := r.Header.Get("X-From-Snap")
	if toSnap == "" {
		respondErr(w, http.StatusBadRequest, errMissingHeader("X-To-Snap"))
		return
	}
	n, err := h.svc.ReceiveDisk(vmID, toSnap, fromSnap, r.Body)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, err)
		return
	}
	bytesReceivedHeader(w, n)
	respondJSON(w, http.StatusOK, map[string]string{"status": "received"})
}

// MemStreamHandler (peer) receives a memory-file range (PUT /migration/{id}/memory).
func (h *Handler) MemStreamHandler(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		respondErr(w, http.StatusUnauthorized, ErrTokenRequired)
		return
	}
	vmID := pathID(r)
	offset, err := strconv.ParseInt(r.Header.Get("X-Offset"), 10, 64)
	if err != nil {
		respondErr(w, http.StatusBadRequest, errMissingHeader("X-Offset"))
		return
	}
	n, err := h.svc.ReceiveMem(vmID, offset, r.Body)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, err)
		return
	}
	bytesReceivedHeader(w, n)
	respondJSON(w, http.StatusOK, map[string]string{"status": "received"})
}

// MemExtentsHandler (peer) receives the data extents of a diff memory
// snapshot (PUT /migration/{id}/memextents).
func (h *Handler) MemExtentsHandler(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		respondErr(w, http.StatusUnauthorized, ErrTokenRequired)
		return
	}
	n, err := h.svc.ReceiveMemExtents(pathID(r), r.Body)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, err)
		return
	}
	bytesReceivedHeader(w, n)
	respondJSON(w, http.StatusOK, map[string]string{"status": "received"})
}

// VMStateHandler (peer) receives the vm_state file (PUT /migration/{id}/vmstate).
func (h *Handler) VMStateHandler(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		respondErr(w, http.StatusUnauthorized, ErrTokenRequired)
		return
	}
	n, err := h.svc.ReceiveVMState(pathID(r), r.Body)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, err)
		return
	}
	bytesReceivedHeader(w, n)
	respondJSON(w, http.StatusOK, map[string]string{"status": "received"})
}

// FileHandler (peer) receives an aux per-VM file (PUT /migration/{id}/file).
func (h *Handler) FileHandler(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		respondErr(w, http.StatusUnauthorized, ErrTokenRequired)
		return
	}
	rel := r.Header.Get("X-File-Path")
	if rel == "" {
		respondErr(w, http.StatusBadRequest, errMissingHeader("X-File-Path"))
		return
	}
	n, err := h.svc.ReceiveFile(pathID(r), rel, r.Body)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, err)
		return
	}
	bytesReceivedHeader(w, n)
	respondJSON(w, http.StatusOK, map[string]string{"status": "received"})
}

// RestoreHandler (peer) performs the cutover boot (POST /migration/{id}/restore).
func (h *Handler) RestoreHandler(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		respondErr(w, http.StatusUnauthorized, ErrTokenRequired)
		return
	}
	vmID := pathID(r)
	var payload CutoverPayload
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload)
	}
	if err := h.svc.Restore(vmID, &payload); err != nil {
		respondErr(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

// ConfirmHandler (peer) acknowledges the VM now lives here (POST /migration/{id}/confirm).
func (h *Handler) ConfirmHandler(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		respondErr(w, http.StatusUnauthorized, ErrTokenRequired)
		return
	}
	if err := h.svc.ConfirmIncoming(pathID(r)); err != nil {
		respondErr(w, http.StatusConflict, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

// AbortIncomingHandler (peer) tears down a received-but-unconfirmed migration
// (POST /migration/{id}/abort).
func (h *Handler) AbortIncomingHandler(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		respondErr(w, http.StatusUnauthorized, ErrTokenRequired)
		return
	}
	if err := h.svc.AbortIncoming(pathID(r)); err != nil {
		respondErr(w, http.StatusConflict, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

// IncomingStatusHandler (peer) reports target-side progress (GET /migration/{id}/status).
func (h *Handler) IncomingStatusHandler(w http.ResponseWriter, r *http.Request) {
	st, ok := h.svc.IncomingStatus(pathID(r))
	if !ok {
		respondErr(w, http.StatusNotFound, ErrNoPending)
		return
	}
	respondJSON(w, http.StatusOK, st)
}

// MigrateHandler (portal) starts a live migration (POST /vms/{id}/migrate).
func (h *Handler) MigrateHandler(w http.ResponseWriter, r *http.Request) {
	var req MigrationRequest
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			respondErr(w, http.StatusBadRequest, err)
			return
		}
	}
	st, err := h.svc.StartLive(pathID(r), &req)
	if err != nil {
		respondErr(w, http.StatusConflict, err)
		return
	}
	respondJSON(w, http.StatusAccepted, st)
}

// OutgoingStatusHandler (portal) reports source-side progress (GET /vms/{id}/migration).
func (h *Handler) OutgoingStatusHandler(w http.ResponseWriter, r *http.Request) {
	st, ok := h.svc.OutgoingStatus(pathID(r))
	if !ok {
		respondErr(w, http.StatusNotFound, ErrNoPending)
		return
	}
	respondJSON(w, http.StatusOK, st)
}

// AbortOutgoingHandler (portal) aborts a migration started here
// (POST /vms/{id}/migration/abort).
func (h *Handler) AbortOutgoingHandler(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.AbortSource(pathID(r)); err != nil {
		respondErr(w, http.StatusConflict, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

func errMissingHeader(name string) error {
	return &missingHeaderError{name: name}
}

type missingHeaderError struct{ name string }

func (e *missingHeaderError) Error() string {
	return "missing required header: " + e.name
}

// pathID extracts the {id} route variable (gorilla/mux).
func pathID(r *http.Request) string {
	return mux.Vars(r)["id"]
}

// ExportHandler (portal) cold-exports a stopped VM (POST /vms/{id}/export).
func (h *Handler) ExportHandler(w http.ResponseWriter, r *http.Request) {
	dir, err := h.svc.ExportVM(pathID(r))
	if err != nil {
		respondErr(w, http.StatusConflict, err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]string{
		"status":     "exported",
		"export_dir": dir,
	})
}

// ImportHandler (portal) cold-imports a VM from an uploaded archive
// (POST /vms/import — body is the tar.gz export).
func (h *Handler) ImportHandler(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.ImportVM(r.Body)
	if err != nil {
		respondErr(w, http.StatusConflict, err)
		return
	}
	respondJSON(w, http.StatusCreated, res)
}

// ListExportsHandler (portal) lists cold exports (GET /exports).
func (h *Handler) ListExportsHandler(w http.ResponseWriter, r *http.Request) {
	names, err := h.svc.ListExports()
	if err != nil {
		respondErr(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"exports": names})
}

// DownloadExportHandler (portal) streams an export as tar.gz
// (GET /exports/{name}).
func (h *Handler) DownloadExportHandler(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q.tar.gz", name))
	if err := h.svc.ServeExportTar(name, w); err != nil {
		respondErr(w, http.StatusNotFound, err)
		return
	}
}

// DeleteExportHandler (portal) removes an export (DELETE /exports/{name}).
func (h *Handler) DeleteExportHandler(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteExport(mux.Vars(r)["name"]); err != nil {
		respondErr(w, http.StatusInternalServerError, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
