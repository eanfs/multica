package handler

// Task-owned artifact staging (Plan C Task 6).
//
// Two task-token endpoints write aurora_artifact_staging rows before Task 7
// commits them as workspace assets:
//
//   - upload streams a sandbox-produced local file through SHA-256 and a
//     per-kind byte limiter into Multica storage, sniffing the first bytes
//     before anything is stored.
//   - import fetches exactly one HTTPS provider result URL through the
//     SSRF-safe client in internal/aurora, revalidating DNS on every redirect,
//     and never persists or logs the URL.
//
// Both insert the staging row only after storage succeeds; a failed insert
// deletes the object it just wrote so an unreferenced object cannot accumulate.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/aurora"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Per-artifact limits from the plan's Artifact Manifest Contract: image, text
// and PDF are 25 MiB; video is 500 MiB. The request cap leaves room for
// multipart framing on top of the largest kind.
const (
	auroraArtifactMaxImageBytes = 25 << 20
	auroraArtifactMaxTextBytes  = 25 << 20
	auroraArtifactMaxPDFBytes   = 25 << 20
	auroraArtifactMaxVideoBytes = 500 << 20

	auroraArtifactMaxRequestBytes  = auroraArtifactMaxVideoBytes + (8 << 20)
	auroraArtifactMaxFieldBytes    = 64 << 10
	auroraArtifactMaxMetadataBytes = 16 << 10
	auroraArtifactImportBodyBytes  = 256 << 10
	auroraArtifactSniffBytes       = 512

	auroraArtifactImportTimeout = 2 * time.Minute
)

// auroraArtifactKind is the server-owned contract for one staged artifact kind.
type auroraArtifactKind struct {
	MaxBytes int64
	MIMEs    []string
}

var auroraArtifactKinds = map[string]auroraArtifactKind{
	"image": {MaxBytes: auroraArtifactMaxImageBytes, MIMEs: []string{"image/png", "image/jpeg", "image/webp", "image/gif"}},
	"text":  {MaxBytes: auroraArtifactMaxTextBytes, MIMEs: []string{"text/plain", "text/markdown", "text/x-markdown"}},
	"pdf":   {MaxBytes: auroraArtifactMaxPDFBytes, MIMEs: []string{"application/pdf"}},
	"video": {MaxBytes: auroraArtifactMaxVideoBytes, MIMEs: []string{"video/mp4", "video/quicktime", "video/webm"}},
}

// auroraArtifactRoles mirrors the manifest writer's role vocabulary.
var auroraArtifactRoles = map[string]bool{"primary": true, "supporting": true, "transcript": true}

// auroraArtifactFormatKinds maps an artifact format onto the kind it belongs to,
// so a declared format that disagrees with its kind is refused before storage.
var auroraArtifactFormatKinds = map[string]string{
	"png": "image", "jpg": "image", "jpeg": "image", "webp": "image", "gif": "image",
	"mp4": "video", "mov": "video", "webm": "video",
	"txt": "text", "md": "text", "markdown": "text",
	"pdf": "pdf",
}

var (
	auroraArtifactIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	auroraArtifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	auroraArtifactSHA256Regex = regexp.MustCompile(`^(?:sha256:)?([0-9a-f]{64})$`)
)

// errAuroraArtifactTooLarge is returned by the digest reader when the stream
// exceeds its per-kind cap, so the caller can distinguish a caller error (413)
// from a storage failure (502).
var errAuroraArtifactTooLarge = errors.New("aurora: artifact exceeds its per-kind byte cap")

// errAuroraArtifactStreamingUnsupported means the configured storage backend
// cannot stream, which is a deployment error rather than a caller error.
var errAuroraArtifactStreamingUnsupported = errors.New("aurora: storage does not support streaming upload")

// auroraArtifactInput is the validated metadata shared by both endpoints.
type auroraArtifactInput struct {
	ManifestArtifactID string
	Name               string
	Kind               string
	Role               string
	Format             string
	MIMEType           string
	SizeBytes          int64
	SHA256             string
	Metadata           []byte
}

// UploadAuroraArtifact streams one sandbox-produced artifact into task-owned
// staging. The multipart metadata fields must precede the file part: the
// per-kind cap is derived from the kind, and a metadata-free stream has no cap.
func (h *Handler) UploadAuroraArtifact(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.auroraTaskScope(w, r)
	if !ok {
		return
	}
	if h.Storage == nil {
		writeFeatureDisabled(w, "aurora_artifact_storage_not_configured", "artifact storage not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, auroraArtifactMaxRequestBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form")
		return
	}

	fields := map[string]string{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid multipart form")
			return
		}
		if part.FormName() != "file" {
			value, readErr := io.ReadAll(io.LimitReader(part, auroraArtifactMaxFieldBytes+1))
			_ = part.Close()
			if readErr != nil || len(value) > auroraArtifactMaxFieldBytes {
				writeError(w, http.StatusBadRequest, "invalid multipart field")
				return
			}
			fields[part.FormName()] = string(value)
			continue
		}
		h.uploadAuroraArtifactFile(w, r, scope, part, fields)
		return
	}
	writeError(w, http.StatusBadRequest, "artifact file is required")
}

func (h *Handler) uploadAuroraArtifactFile(w http.ResponseWriter, r *http.Request, scope auroraTaskScope, part *multipart.Part, fields map[string]string) {
	defer func() { _ = part.Close() }()

	size, ok := parseAuroraArtifactSizeBytes(w, fields["size_bytes"])
	if !ok {
		return
	}
	metadata, ok := normalizeAuroraArtifactMetadataString(w, fields["metadata"])
	if !ok {
		return
	}
	digest, ok := normalizeAuroraArtifactSHA256(w, fields["sha256"])
	if !ok {
		return
	}
	input := auroraArtifactInput{
		ManifestArtifactID: strings.TrimSpace(fields["manifest_artifact_id"]),
		Name:               strings.TrimSpace(fields["name"]),
		Kind:               strings.ToLower(strings.TrimSpace(fields["kind"])),
		Role:               strings.ToLower(strings.TrimSpace(fields["role"])),
		Format:             strings.ToLower(strings.TrimSpace(fields["format"])),
		MIMEType:           normalizeArtifactMIME(fields["mime_type"]),
		SizeBytes:          size,
		SHA256:             digest,
		Metadata:           metadata,
	}
	if input.ManifestArtifactID == "" || !auroraArtifactIDPattern.MatchString(input.ManifestArtifactID) {
		writeError(w, http.StatusBadRequest, "invalid manifest artifact id")
		return
	}
	spec, ok := h.validateAuroraArtifactInput(w, scope.generation.SkillID, input)
	if !ok {
		return
	}

	sniff, err := readAuroraArtifactPrefix(part)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read artifact")
		return
	}
	if len(sniff) == 0 {
		writeError(w, http.StatusBadRequest, "artifact file is empty")
		return
	}
	if !auroraArtifactSniffMatches(w, input.Kind, sniff) {
		return
	}

	stagingID := dbid.NewV7()
	source := io.MultiReader(bytes.NewReader(sniff), part)
	key, stored, storedDigest, err := h.streamAuroraArtifact(r.Context(), stagingID, scope.workspaceID, input.Name, input.MIMEType, spec.MaxBytes, input.SizeBytes, source)
	if err != nil {
		h.deleteAuroraArtifactObject(r.Context(), key)
		if errors.Is(err, errAuroraArtifactTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "artifact exceeds the per-kind size limit")
			return
		}
		slog.Error("aurora artifact upload failed", "task_id", uuidToString(scope.taskID), "error", err)
		writeError(w, http.StatusBadGateway, "failed to store artifact")
		return
	}
	if stored != input.SizeBytes {
		h.deleteAuroraArtifactObject(r.Context(), key)
		writeError(w, http.StatusBadRequest, "artifact size does not match the declared size")
		return
	}
	if storedDigest != input.SHA256 {
		h.deleteAuroraArtifactObject(r.Context(), key)
		writeError(w, http.StatusBadRequest, "artifact sha256 does not match the declared hash")
		return
	}

	row, err := h.insertAuroraArtifactStaging(r.Context(), scope, stagingID, input, "upload", key, stored, storedDigest)
	if err != nil {
		h.deleteAuroraArtifactObject(r.Context(), key)
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "artifact is already staged for this task")
			return
		}
		slog.Error("aurora artifact staging insert failed", "task_id", uuidToString(scope.taskID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record artifact")
		return
	}
	writeJSON(w, http.StatusOK, newAuroraArtifactStagingResponse(row))
}

// auroraArtifactImportRequest is the bounded JSON body of the provider import.
// The field set matches the sandbox broker's createHttpImporter; the manifest
// artifact id, role and format are optional extras for a caller that already
// knows them.
type auroraArtifactImportRequest struct {
	URL                string          `json:"url"`
	Kind               string          `json:"kind"`
	Name               string          `json:"name"`
	MIMEType           string          `json:"mime_type"`
	SizeBytes          *int64          `json:"size_bytes"`
	Metadata           json.RawMessage `json:"metadata"`
	ManifestArtifactID string          `json:"manifest_artifact_id"`
	Role               string          `json:"role"`
	Format             string          `json:"format"`
}

// ImportAuroraArtifact fetches one provider result URL into task-owned staging.
// The URL is never persisted and never appears in a response or a log line.
func (h *Handler) ImportAuroraArtifact(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.auroraTaskScope(w, r)
	if !ok {
		return
	}
	if h.Storage == nil {
		writeFeatureDisabled(w, "aurora_artifact_storage_not_configured", "artifact storage not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, auroraArtifactImportBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body) > auroraArtifactImportBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var req auroraArtifactImportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	metadata, ok := normalizeAuroraArtifactMetadataRaw(w, req.Metadata)
	if !ok {
		return
	}
	input := auroraArtifactInput{
		ManifestArtifactID: strings.TrimSpace(req.ManifestArtifactID),
		Name:               strings.TrimSpace(req.Name),
		Kind:               strings.ToLower(strings.TrimSpace(req.Kind)),
		Role:               strings.ToLower(strings.TrimSpace(req.Role)),
		Format:             strings.ToLower(strings.TrimSpace(req.Format)),
		MIMEType:           normalizeArtifactMIME(req.MIMEType),
		Metadata:           metadata,
	}
	var declaredSize *int64
	if req.SizeBytes != nil {
		input.SizeBytes = *req.SizeBytes
		declaredSize = req.SizeBytes
	}
	if input.ManifestArtifactID != "" && !auroraArtifactIDPattern.MatchString(input.ManifestArtifactID) {
		writeError(w, http.StatusBadRequest, "invalid manifest artifact id")
		return
	}
	spec, ok := h.validateAuroraArtifactInput(w, scope.generation.SkillID, input)
	if !ok {
		return
	}
	source, ok := parseAuroraArtifactSourceURL(w, req.URL)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), auroraArtifactImportTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, source.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid artifact source URL")
		return
	}
	resp, err := h.auroraArtifactImportHTTPClient().Do(httpReq)
	if err != nil {
		writeAuroraArtifactImportFetchError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, "artifact source returned a non-200 response")
		return
	}
	if resp.ContentLength > spec.MaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "artifact source exceeds the per-kind size limit")
		return
	}
	sniff, err := readAuroraArtifactPrefix(resp.Body)
	if err != nil {
		writeAuroraArtifactImportFetchError(w, err)
		return
	}
	if len(sniff) == 0 {
		writeError(w, http.StatusBadRequest, "artifact source is empty")
		return
	}
	if !auroraArtifactSniffMatches(w, input.Kind, sniff) {
		return
	}

	contentLength := resp.ContentLength
	if contentLength <= 0 && declaredSize != nil && *declaredSize > 0 {
		contentLength = *declaredSize
	}
	stagingID := dbid.NewV7()
	key, stored, storedDigest, err := h.streamAuroraArtifact(ctx, stagingID, scope.workspaceID, input.Name, input.MIMEType, spec.MaxBytes, contentLength,
		io.MultiReader(bytes.NewReader(sniff), resp.Body))
	if err != nil {
		h.deleteAuroraArtifactObject(ctx, key)
		if errors.Is(err, errAuroraArtifactTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "artifact source exceeds the per-kind size limit")
			return
		}
		slog.Error("aurora artifact import store failed", "task_id", uuidToString(scope.taskID), "error", err)
		writeError(w, http.StatusBadGateway, "failed to store artifact")
		return
	}
	if declaredSize != nil && stored != *declaredSize {
		h.deleteAuroraArtifactObject(ctx, key)
		writeError(w, http.StatusBadRequest, "artifact size does not match the declared size")
		return
	}

	row, err := h.insertAuroraArtifactStaging(r.Context(), scope, stagingID, input, "provider_import", key, stored, storedDigest)
	if err != nil {
		h.deleteAuroraArtifactObject(r.Context(), key)
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "artifact is already staged for this task")
			return
		}
		slog.Error("aurora artifact staging insert failed", "task_id", uuidToString(scope.taskID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record artifact")
		return
	}
	writeJSON(w, http.StatusOK, newAuroraArtifactStagingResponse(row))
}

// auroraArtifactImportHTTPClient returns the test override or the production
// SSRF-safe client.
func (h *Handler) auroraArtifactImportHTTPClient() *http.Client {
	if h.auroraArtifactImportClient != nil {
		return h.auroraArtifactImportClient
	}
	return aurora.NewArtifactImportClient(nil, nil)
}

// validateAuroraArtifactInput enforces the server-owned artifact policy: the
// kind must be one the skill produces, the name/role/format must be well formed,
// the declared MIME must be accepted for the kind, and the declared size must
// fit the kind's cap.
func (h *Handler) validateAuroraArtifactInput(w http.ResponseWriter, skillID string, input auroraArtifactInput) (auroraArtifactKind, bool) {
	spec, ok := auroraArtifactKinds[input.Kind]
	if !ok {
		writeError(w, http.StatusBadRequest, "unsupported artifact kind")
		return auroraArtifactKind{}, false
	}
	policy, ok := aurora.ExecutionPolicy(skillID)
	if !ok || !slices.Contains(policy.OutputKinds, input.Kind) {
		writeError(w, http.StatusBadRequest, "artifact kind is not produced by this skill")
		return auroraArtifactKind{}, false
	}
	if !auroraArtifactNamePattern.MatchString(input.Name) || strings.Contains(input.Name, "..") {
		writeError(w, http.StatusBadRequest, "invalid artifact name")
		return auroraArtifactKind{}, false
	}
	if input.Role != "" && !auroraArtifactRoles[input.Role] {
		writeError(w, http.StatusBadRequest, "invalid artifact role")
		return auroraArtifactKind{}, false
	}
	if input.Format != "" {
		formatKind, ok := auroraArtifactFormatKinds[input.Format]
		if !ok || formatKind != input.Kind {
			writeError(w, http.StatusBadRequest, "artifact format does not match its kind")
			return auroraArtifactKind{}, false
		}
	}
	if !slices.Contains(spec.MIMEs, input.MIMEType) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported artifact MIME type")
		return auroraArtifactKind{}, false
	}
	if input.SizeBytes < 0 || input.SizeBytes > spec.MaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "artifact exceeds the per-kind size limit")
		return auroraArtifactKind{}, false
	}
	return spec, true
}

// streamAuroraArtifact keys and streams one artifact, hashing and capping the
// bytes as they pass. It returns the object key even on failure so the caller
// can delete a partial object.
func (h *Handler) streamAuroraArtifact(ctx context.Context, stagingID, workspaceID pgtype.UUID, name, mimeType string, maxBytes, contentLength int64, source io.Reader) (string, int64, string, error) {
	uploader, ok := h.Storage.(sourceContextStreamUploader)
	if !ok {
		return "", 0, "", errAuroraArtifactStreamingUnsupported
	}
	key := "workspaces/" + uuidToString(workspaceID) + "/aurora-artifacts/" + uuidToString(stagingID) + "/" + name
	reader := &auroraArtifactDigestReader{source: source, hash: sha256.New(), remaining: maxBytes}
	if _, err := uploader.UploadStream(ctx, key, reader, contentLength, mimeType, name); err != nil {
		return key, reader.read, "", err
	}
	return key, reader.read, "sha256:" + hex.EncodeToString(reader.hash.Sum(nil)), nil
}

// insertAuroraArtifactStaging writes the staging row. A unique violation means
// the (task, manifest artifact id) pair is already staged.
func (h *Handler) insertAuroraArtifactStaging(ctx context.Context, scope auroraTaskScope, stagingID pgtype.UUID, input auroraArtifactInput, sourceType, key string, size int64, digest string) (db.AuroraArtifactStaging, error) {
	params := db.CreateAuroraArtifactStagingParams{
		ID:           stagingID,
		TaskID:       scope.taskID,
		GenerationID: scope.generation.ID,
		WorkspaceID:  scope.workspaceID,
		StorageKey:   key,
		Name:         input.Name,
		Kind:         input.Kind,
		MimeType:     input.MIMEType,
		SizeBytes:    size,
		Sha256:       digest,
		Metadata:     input.Metadata,
		SourceType:   sourceType,
	}
	if input.ManifestArtifactID != "" {
		params.ManifestArtifactID = pgtype.Text{String: input.ManifestArtifactID, Valid: true}
	}
	if input.Role != "" {
		params.Role = pgtype.Text{String: input.Role, Valid: true}
	}
	if input.Format != "" {
		params.Format = pgtype.Text{String: input.Format, Valid: true}
	}
	return h.Queries.CreateAuroraArtifactStaging(ctx, params)
}

// deleteAuroraArtifactObject removes an object whose row was never written. The
// key is workspace/staging/name only, so logging it cannot leak a provider URL.
func (h *Handler) deleteAuroraArtifactObject(ctx context.Context, key string) {
	if key == "" || h.Storage == nil {
		return
	}
	if err := h.Storage.DeleteObject(ctx, key); err != nil {
		slog.Error("failed to delete unreferenced aurora artifact object", "key", key, "error", err)
	}
}

// auroraArtifactStagingResponse is the wire shape both endpoints return. The
// broker reads staging_id, size_bytes and sha256.
type auroraArtifactStagingResponse struct {
	StagingID          string          `json:"staging_id"`
	ID                 string          `json:"id"`
	TaskID             string          `json:"task_id"`
	GenerationID       string          `json:"generation_id"`
	ManifestArtifactID *string         `json:"manifest_artifact_id"`
	Name               string          `json:"name"`
	Kind               string          `json:"kind"`
	Role               *string         `json:"role"`
	Format             *string         `json:"format"`
	MIMEType           string          `json:"mime_type"`
	SizeBytes          int64           `json:"size_bytes"`
	SHA256             string          `json:"sha256"`
	Metadata           json.RawMessage `json:"metadata"`
	SourceType         string          `json:"source_type"`
	Status             string          `json:"status"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

func newAuroraArtifactStagingResponse(row db.AuroraArtifactStaging) auroraArtifactStagingResponse {
	resp := auroraArtifactStagingResponse{
		StagingID:    uuidToString(row.ID),
		ID:           uuidToString(row.ID),
		TaskID:       uuidToString(row.TaskID),
		GenerationID: uuidToString(row.GenerationID),
		Name:         row.Name,
		Kind:         row.Kind,
		MIMEType:     row.MimeType,
		SizeBytes:    row.SizeBytes,
		SHA256:       row.Sha256,
		SourceType:   row.SourceType,
		Status:       row.Status,
		CreatedAt:    row.CreatedAt.Time,
		UpdatedAt:    row.UpdatedAt.Time,
	}
	if row.ManifestArtifactID.Valid {
		value := row.ManifestArtifactID.String
		resp.ManifestArtifactID = &value
	}
	if row.Role.Valid {
		value := row.Role.String
		resp.Role = &value
	}
	if row.Format.Valid {
		value := row.Format.String
		resp.Format = &value
	}
	if len(row.Metadata) > 0 {
		resp.Metadata = json.RawMessage(row.Metadata)
	} else {
		resp.Metadata = json.RawMessage("{}")
	}
	return resp
}

// auroraArtifactDigestReader hashes and caps the bytes it reads. Passing the cap
// means an oversized stream fails without landing an over-cap object, rather
// than being truncated into a decodable prefix.
type auroraArtifactDigestReader struct {
	source    io.Reader
	hash      hash.Hash
	remaining int64
	read      int64
}

func (r *auroraArtifactDigestReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n > 0 {
			return 0, errAuroraArtifactTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.source.Read(p)
	if n > 0 {
		_, _ = r.hash.Write(p[:n])
		r.read += int64(n)
		r.remaining -= int64(n)
	}
	return n, err
}

// readAuroraArtifactPrefix reads the bounded prefix used for MIME sniffing. A
// short file is not an error.
func readAuroraArtifactPrefix(source io.Reader) ([]byte, error) {
	buf := make([]byte, auroraArtifactSniffBytes)
	n, err := io.ReadFull(source, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:n], nil
}

// auroraArtifactSniffMatches rejects a body whose sniffed family is not the
// declared kind. The declared MIME was already checked against the kind's
// allowlist; this is the content half of the same check.
func auroraArtifactSniffMatches(w http.ResponseWriter, kind string, sniff []byte) bool {
	sniffed := normalizeArtifactMIME(http.DetectContentType(sniff))
	sniffKind, ok := auroraArtifactKindForMIME(sniffed)
	if !ok || sniffKind != kind {
		writeError(w, http.StatusUnsupportedMediaType, "artifact content does not match its declared kind")
		return false
	}
	return true
}

func auroraArtifactKindForMIME(contentType string) (string, bool) {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return "image", true
	case strings.HasPrefix(contentType, "video/"):
		return "video", true
	case contentType == "application/pdf":
		return "pdf", true
	case strings.HasPrefix(contentType, "text/"):
		return "text", true
	}
	return "", false
}

// normalizeArtifactMIME strips parameters and casing so "text/plain; charset=utf-8"
// compares as "text/plain".
func normalizeArtifactMIME(raw string) string {
	if i := strings.IndexByte(raw, ';'); i >= 0 {
		raw = raw[:i]
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

func parseAuroraArtifactSizeBytes(w http.ResponseWriter, raw string) (int64, bool) {
	trimmed := strings.TrimSpace(raw)
	size, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || size < 0 {
		writeError(w, http.StatusBadRequest, "invalid artifact size_bytes")
		return 0, false
	}
	return size, true
}

func normalizeAuroraArtifactSHA256(w http.ResponseWriter, raw string) (string, bool) {
	match := auroraArtifactSHA256Regex.FindStringSubmatch(strings.ToLower(strings.TrimSpace(raw)))
	if match == nil {
		writeError(w, http.StatusBadRequest, "invalid artifact sha256")
		return "", false
	}
	return "sha256:" + match[1], true
}

func normalizeAuroraArtifactMetadataString(w http.ResponseWriter, raw string) ([]byte, bool) {
	return normalizeAuroraArtifactMetadataRaw(w, json.RawMessage(strings.TrimSpace(raw)))
}

func normalizeAuroraArtifactMetadataRaw(w http.ResponseWriter, raw json.RawMessage) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []byte("{}"), true
	}
	if len(trimmed) > auroraArtifactMaxMetadataBytes {
		writeError(w, http.StatusBadRequest, "artifact metadata is too large")
		return nil, false
	}
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		writeError(w, http.StatusBadRequest, "artifact metadata must be a JSON object")
		return nil, false
	}
	out := make([]byte, len(trimmed))
	copy(out, trimmed)
	return out, true
}

// parseAuroraArtifactSourceURL validates exactly one HTTPS URL. Credentials and
// non-HTTPS schemes are refused before any DNS lookup.
func parseAuroraArtifactSourceURL(w http.ResponseWriter, raw string) (*url.URL, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		writeError(w, http.StatusBadRequest, "artifact source url is required")
		return nil, false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil {
		writeError(w, http.StatusBadRequest, "artifact source must be a single public HTTPS url without credentials")
		return nil, false
	}
	return parsed, true
}

// writeAuroraArtifactImportFetchError maps a fetch failure without ever
// rendering the URL: net/http errors embed the full request URL, which may be a
// signed provider URL.
func writeAuroraArtifactImportFetchError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, aurora.ErrArtifactAddrBlocked), errors.Is(err, aurora.ErrArtifactRedirectRefused):
		writeError(w, http.StatusBadRequest, "artifact source url is not allowed")
	case isAuroraArtifactTimeout(err):
		writeError(w, http.StatusGatewayTimeout, "artifact source timed out")
	default:
		writeError(w, http.StatusBadGateway, "failed to fetch artifact source")
	}
}

func isAuroraArtifactTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
