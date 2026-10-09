package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/google/uuid"
)

const (
	// GeminiCaptureTargetModel is intentionally fixed. This trace is a temporary
	// incident tool, not a general body logging switch.
	GeminiCaptureTargetModel = "gemini-3.8-flash"

	geminiCaptureSchemaVersion     = 1
	geminiCaptureLeaseEnv          = "GATEWAY_GEMINI_CAPTURE_LEASE_FILE"
	geminiCaptureLeaseMaxBytes     = 64 * 1024
	geminiCaptureMaxBytes          = 64 << 20
	geminiCaptureQueueBytes        = 4 << 20
	geminiCaptureDiskBytes         = 128 << 20 // per-request artifact budget
	geminiCaptureOutputBytes       = 512 << 20 // shared output-dir budget
	geminiCaptureParserMaxBytes    = 1 << 20
	geminiCaptureQueueDepth        = 256
	geminiCaptureLeasePoll         = 250 * time.Millisecond
	geminiCaptureCloseTimeout      = 500 * time.Millisecond
	geminiCaptureManifestTimeout   = 500 * time.Millisecond
	geminiCaptureAbortWorkerCount  = 4
	geminiCaptureManifestQueueSize = 8
	geminiCaptureWriterSlots       = 8
	geminiCaptureFailureWorkers    = 2
	geminiCaptureFailureQueueSize  = 8
	geminiCaptureManifestReserve   = 4096
)

// GeminiCaptureLease is the only runtime control surface for the temporary
// capture. It is read-only from the process: an operator arms, disarms, or
// renews it by replacing this small JSON file.
type GeminiCaptureLease struct {
	Enabled         bool     `json:"enabled"`
	MetadataUserIDs []string `json:"metadata_user_ids"`
	Model           string   `json:"model"`
	ExpiresAt       string   `json:"expires_at"`
	OutputDir       string   `json:"output_dir"`
}

// GeminiCapture is a cheap, disabled-by-default controller. A configured path
// alone does not enable capture; the lease must also validate at request time.
type GeminiCapture struct {
	leasePath string
	budgetMu  sync.Mutex
	budgets   map[string]*geminiCaptureOutputBudget
}

// GeminiCaptureRequest is request-scoped state carried through the gateway
// context. It is deliberately not exported as a generic tracing interface.
type GeminiCaptureRequest struct {
	controller *GeminiCapture
	ctx        context.Context

	mu sync.Mutex

	candidate        bool
	selectedAG       bool
	modelChecked     bool
	modelMatched     bool
	activated        bool
	finalized        bool
	captureEnabled   bool
	incompleteReason map[string]struct{}

	metadataUserID    string
	requestModel      string
	stream            bool
	userID            int64
	groupID           int64
	selectedAccountID int64
	requestID         string
	clientRequestID   string
	startedAtTime     time.Time

	inboundRedacted      []byte
	inboundRedactionOK   bool
	redactedFields       []string
	inboundTruncated     bool
	artifactDir          string
	lease                GeminiCaptureLease
	leaseExpiresAt       time.Time
	lastLeaseCheck       time.Time
	leaseRefreshInFlight bool

	queue              *geminiCaptureQueue
	sanitizers         map[string]*geminiCaptureStreamSanitizer
	attempts           []*GeminiCaptureAttempt
	nextSeq            int
	terminals          map[string]struct{}
	usage              map[string]any
	parseBuffer        string
	parserDisabled     bool
	clientDisconnect   bool
	ctxCanceled        bool
	timeout            bool
	upstreamReadError  bool
	upstreamEOF        bool
	gatewayStatus      int
	convertedWritten   bool
	budgetReservation  int64
	budget             *geminiCaptureOutputBudget
	localFailureWorker bool
}

// GeminiCaptureAttempt identifies one actual HTTP attempt. Response IDs are
// kept here; they are never conflated with the gateway request ID.
type GeminiCaptureAttempt struct {
	trace        *GeminiCaptureRequest
	sequence     int
	accountID    int64
	groupID      int64
	requestFile  string
	responseFile string

	mu                sync.Mutex
	statusCode        int
	upstreamRequestID string
	responseEOF       bool
	responseClosed    bool
	responseReadError bool
	clientDisconnect  bool
	ctxCanceled       bool
	timeout           bool
	requestError      string
}

type geminiCaptureFile struct {
	file     *os.File
	path     string
	bytes    int64
	expected int64
	complete bool
}

type geminiCaptureChunk struct {
	target string
	data   []byte
}

type geminiCaptureStreamSanitizer struct {
	trace    *GeminiCaptureRequest
	target   string
	mu       sync.Mutex
	pending  []byte
	disabled bool
}

// geminiCaptureQueue makes all body/file writes asynchronous and bounded. A
// full queue fails capture open rather than delaying a provider stream.
var (
	geminiCaptureAbortOnce     sync.Once
	geminiCaptureAbortQueue    chan []*os.File
	geminiCaptureManifestOnce  sync.Once
	geminiCaptureManifestQueue chan *geminiCaptureManifestTask
	geminiCaptureWriterOnce    sync.Once
	geminiCaptureWriterSlotsCh chan struct{}
	geminiCaptureFailureOnce   sync.Once
	geminiCaptureFailureQueue  chan *GeminiCaptureRequest
)

type geminiCaptureManifestTask struct {
	path       string
	body       []byte
	cancelled  chan struct{}
	cancelOnce sync.Once
	done       chan error
}

type geminiCaptureOutputBudget struct {
	mu      sync.Mutex
	root    string
	used    int64
	ready   bool
	blocked bool
}

type geminiCaptureQueue struct {
	mu             sync.Mutex
	items          chan geminiCaptureChunk
	closed         bool
	drop           bool
	pendingBytes   int64
	totalBytes     int64
	budget         *geminiCaptureOutputBudget
	done           chan struct{}
	writerAdmitted bool
	incomplete     map[string]struct{}
	files          map[string]*geminiCaptureFile
	wg             sync.WaitGroup
}

func newGeminiCaptureQueue(budgets ...*geminiCaptureOutputBudget) *geminiCaptureQueue {
	var budget *geminiCaptureOutputBudget
	if len(budgets) > 0 {
		budget = budgets[0]
	}
	q := &geminiCaptureQueue{
		items:      make(chan geminiCaptureChunk, geminiCaptureQueueDepth),
		budget:     budget,
		done:       make(chan struct{}),
		incomplete: make(map[string]struct{}),
		files:      make(map[string]*geminiCaptureFile),
	}
	q.wg.Add(1)
	q.acquireWriter()
	return q
}

func (q *geminiCaptureQueue) acquireWriter() {
	geminiCaptureWriterOnce.Do(func() {
		geminiCaptureWriterSlotsCh = make(chan struct{}, geminiCaptureWriterSlots)
	})
	select {
	case geminiCaptureWriterSlotsCh <- struct{}{}:
		q.writerAdmitted = true
		go q.run()
	default:
		q.incomplete["writer_admission"] = struct{}{}
		q.drop = true
		q.wg.Done()
		close(q.done)
	}
}

func (q *geminiCaptureQueue) run() {
	defer q.wg.Done()
	defer close(q.done)
	defer func() { <-geminiCaptureWriterSlotsCh }()
	defer q.finishFiles()
	for item := range q.items {
		q.mu.Lock()
		q.pendingBytes -= int64(len(item.data))
		file := q.files[item.target]
		q.mu.Unlock()
		if file == nil {
			q.markIncomplete(item.target)
			continue
		}
		n, err := file.file.Write(item.data)
		q.mu.Lock()
		file.bytes += int64(n)
		if err != nil || n != len(item.data) {
			file.complete = false
			q.incomplete[item.target] = struct{}{}
			q.drop = true
		}
		q.mu.Unlock()
	}
}

func (q *geminiCaptureQueue) finishFiles() {
	q.mu.Lock()
	type fileEntry struct {
		target string
		file   *geminiCaptureFile
	}
	files := make([]fileEntry, 0, len(q.files))
	for target, file := range q.files {
		files = append(files, fileEntry{target: target, file: file})
	}
	q.mu.Unlock()
	for _, entry := range files {
		if entry.file.file == nil {
			continue
		}
		if err := entry.file.file.Sync(); err != nil {
			q.markFileIncomplete(entry.target, "file_sync_failed")
		}
		if err := entry.file.file.Close(); err != nil {
			q.markFileIncomplete(entry.target, "file_close_failed")
		}
	}
}

func (q *geminiCaptureQueue) addFile(target, path string, f *os.File) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.files[target] = &geminiCaptureFile{file: f, path: path, complete: true}
}

func (q *geminiCaptureQueue) markIncomplete(reason string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.incomplete == nil {
		q.incomplete = make(map[string]struct{})
	}
	q.incomplete[reason] = struct{}{}
	q.drop = true
}

func (q *geminiCaptureQueue) markFileIncomplete(target, reason string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.incomplete == nil {
		q.incomplete = make(map[string]struct{})
	}
	if file := q.files[target]; file != nil {
		file.complete = false
	}
	q.incomplete[reason] = struct{}{}
}

func (q *geminiCaptureQueue) enqueue(target string, p []byte, expected int) bool {
	if len(p) == 0 && expected == 0 {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.incomplete == nil {
		q.incomplete = make(map[string]struct{})
	}
	if q.closed || q.drop {
		if expected > 0 {
			q.incomplete["capture_disabled_or_overflow"] = struct{}{}
		}
		return false
	}
	if expected < 0 {
		expected = 0
	}
	file := q.files[target]
	if file == nil {
		q.incomplete["unknown_file"] = struct{}{}
		q.drop = true
		return false
	}
	dataLen := int64(len(p))
	if q.pendingBytes+dataLen > geminiCaptureQueueBytes {
		q.incomplete["queue_quota"] = struct{}{}
		q.drop = true
		return false
	}
	if q.totalBytes+dataLen > geminiCaptureDiskBytes {
		q.incomplete["disk_quota"] = struct{}{}
		q.drop = true
		file.complete = false
		return false
	}
	if q.budget != nil && !q.budget.reserve(dataLen) {
		q.incomplete["shared_budget"] = struct{}{}
		q.drop = true
		file.complete = false
		return false
	}
	copyBytes := append([]byte(nil), p...)
	file.expected += int64(expected)
	if expected > len(copyBytes) {
		file.complete = false
	}
	if expected > 0 && len(copyBytes) == 0 {
		file.complete = false
	}
	select {
	case q.items <- geminiCaptureChunk{target: target, data: copyBytes}:
		q.pendingBytes += dataLen
		q.totalBytes += dataLen
		return true
	default:
		q.incomplete["queue_full"] = struct{}{}
		q.drop = true
		file.complete = false
		if q.budget != nil {
			q.budget.release(dataLen)
		}
		return false
	}
}

func startGeminiCaptureFailureWorkers() {
	geminiCaptureFailureOnce.Do(func() {
		geminiCaptureFailureQueue = make(chan *GeminiCaptureRequest, geminiCaptureFailureQueueSize)
		for i := 0; i < geminiCaptureFailureWorkers; i++ {
			go func() {
				for capture := range geminiCaptureFailureQueue {
					capture.mu.Lock()
					capture.localFailureWorker = true
					capture.finalized = false
					status := capture.gatewayStatus
					capture.mu.Unlock()
					capture.Finish(status)
				}
			}()
		}
	})
}

func enqueueGeminiCaptureFailure(capture *GeminiCaptureRequest) bool {
	startGeminiCaptureFailureWorkers()
	select {
	case geminiCaptureFailureQueue <- capture:
		return true
	default:
		return false
	}
}

func enqueueGeminiCaptureAbort(files []*os.File) bool {
	geminiCaptureAbortOnce.Do(func() {
		geminiCaptureAbortQueue = make(chan []*os.File, geminiCaptureAbortWorkerCount)
		for i := 0; i < geminiCaptureAbortWorkerCount; i++ {
			go func() {
				for batch := range geminiCaptureAbortQueue {
					for _, file := range batch {
						_ = file.Close()
					}
				}
			}()
		}
	})
	select {
	case geminiCaptureAbortQueue <- files:
		return true
	default:
		return false
	}
}

func (q *geminiCaptureQueue) close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	close(q.items)
	q.mu.Unlock()

	waitDone := func() <-chan struct{} {
		if q.done != nil {
			return q.done
		}
		done := make(chan struct{})
		go func() {
			q.wg.Wait()
			close(done)
		}()
		return done
	}
	timer := time.NewTimer(geminiCaptureCloseTimeout)
	defer timer.Stop()
	select {
	case <-waitDone():
	case <-timer.C:
		q.markIncomplete("close_timeout")
		q.mu.Lock()
		files := make([]*os.File, 0, len(q.files))
		for _, file := range q.files {
			if file.file != nil {
				files = append(files, file.file)
			}
		}
		q.mu.Unlock()
		if !enqueueGeminiCaptureAbort(files) {
			q.markIncomplete("abort_queue_full")
		}
	}
}

func (q *geminiCaptureQueue) snapshot() (map[string]GeminiCaptureFileEvidence, []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	files := make(map[string]GeminiCaptureFileEvidence, len(q.files))
	for target, file := range q.files {
		complete := file.complete && file.bytes == file.expected
		files[target] = GeminiCaptureFileEvidence{
			Path:     file.path,
			Bytes:    file.bytes,
			Expected: file.expected,
			Complete: complete,
		}
	}
	reasons := make([]string, 0, len(q.incomplete))
	for reason := range q.incomplete {
		reasons = append(reasons, reason)
	}
	return files, reasons
}

// GeminiCaptureFileEvidence is a relative artifact path and completeness
// record. It intentionally contains no headers or credential material.
type GeminiCaptureFileEvidence struct {
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	Expected int64  `json:"expected_bytes"`
	Complete bool   `json:"complete"`
}

type geminiCaptureAttemptEvidence struct {
	Sequence          int    `json:"sequence"`
	AccountID         int64  `json:"account_id"`
	GroupID           int64  `json:"group_id,omitempty"`
	StatusCode        int    `json:"status_code"`
	Outcome           string `json:"outcome"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	RequestFile       string `json:"request_file,omitempty"`
	ResponseFile      string `json:"response_file,omitempty"`
	ResponseEOF       bool   `json:"response_eof"`
	ResponseComplete  bool   `json:"response_complete"`
	ResponseReadError bool   `json:"response_read_error"`
	RequestError      string `json:"request_error,omitempty"`
	ClientDisconnect  bool   `json:"client_disconnect"`
	ContextCanceled   bool   `json:"context_canceled"`
	Timeout           bool   `json:"timeout"`
}

type geminiCaptureManifest struct {
	SchemaVersion         int                                  `json:"schema_version"`
	StartedAt             string                               `json:"started_at"`
	FinishedAt            string                               `json:"finished_at"`
	RequestID             string                               `json:"request_id,omitempty"`
	ClientRequestID       string                               `json:"client_request_id,omitempty"`
	UserID                int64                                `json:"user_id,omitempty"`
	GroupID               int64                                `json:"group_id,omitempty"`
	SelectedAccountID     int64                                `json:"selected_account_id,omitempty"`
	MetadataUserID        string                               `json:"metadata_user_id"`
	RequestPath           string                               `json:"request_path"`
	RequestModel          string                               `json:"request_model"`
	FinalModel            string                               `json:"final_model,omitempty"`
	Stream                bool                                 `json:"stream"`
	AGChain               bool                                 `json:"ag_chain"`
	GatewayStatus         int                                  `json:"gateway_status"`
	Outcome               string                               `json:"outcome"`
	UpstreamAttempts      int                                  `json:"upstream_attempts"`
	UpstreamFinishReasons []string                             `json:"upstream_finish_reasons,omitempty"`
	Usage                 map[string]any                       `json:"usage,omitempty"`
	Termination           []string                             `json:"termination,omitempty"`
	Incomplete            bool                                 `json:"incomplete"`
	IncompleteReasons     []string                             `json:"incomplete_reasons,omitempty"`
	RedactedFields        []string                             `json:"redacted_fields,omitempty"`
	HeadersRecorded       bool                                 `json:"headers_recorded"`
	CredentialHandling    string                               `json:"credential_handling"`
	Files                 map[string]GeminiCaptureFileEvidence `json:"files"`
	Attempts              []geminiCaptureAttemptEvidence       `json:"attempts"`
}

// NewGeminiCapture creates a controller using the configured lease path. The
// environment fallback keeps directly constructed test Config values useful;
// normal deployments use the Viper-bound config field.
func NewGeminiCapture(cfg *config.Config) *GeminiCapture {
	path := ""
	if cfg != nil {
		path = strings.TrimSpace(cfg.Gateway.GeminiCaptureLeaseFile)
	}
	if path == "" {
		path = strings.TrimSpace(os.Getenv(geminiCaptureLeaseEnv))
	}
	return &GeminiCapture{leasePath: path, budgets: make(map[string]*geminiCaptureOutputBudget)}
}

func (g *GeminiCapture) outputBudget(root string) *geminiCaptureOutputBudget {
	if g == nil {
		return nil
	}
	root = filepath.Clean(root)
	g.budgetMu.Lock()
	defer g.budgetMu.Unlock()
	if g.budgets == nil {
		g.budgets = make(map[string]*geminiCaptureOutputBudget)
	}
	if budget := g.budgets[root]; budget != nil {
		return budget
	}
	budget := &geminiCaptureOutputBudget{root: root}
	g.budgets[root] = budget
	entries, err := os.ReadDir(root)
	if err == nil && len(entries) == 0 {
		budget.ready = true
		return budget
	}
	go budget.scanExisting()
	return budget
}

func (b *geminiCaptureOutputBudget) scanExisting() {
	var total int64
	blocked := false
	err := filepath.WalkDir(b.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if total > geminiCaptureOutputBytes {
			blocked = true
			return filepath.SkipAll
		}
		return nil
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil && !blocked {
		b.blocked = true
	} else {
		b.used = total
		b.blocked = blocked
	}
	b.ready = true
}

func (b *geminiCaptureOutputBudget) readyForCapture() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ready && !b.blocked && b.used < geminiCaptureOutputBytes
}

func (b *geminiCaptureOutputBudget) reserve(size int64) bool {
	if b == nil || size <= 0 {
		return b != nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.ready || b.blocked || b.used+size > geminiCaptureOutputBytes {
		return false
	}
	b.used += size
	return true
}

func (b *geminiCaptureOutputBudget) release(size int64) {
	if b == nil || size <= 0 {
		return
	}
	b.mu.Lock()
	if b.used >= size {
		b.used -= size
	} else {
		b.used = 0
	}
	b.mu.Unlock()
}

// Begin matches only the legal Messages endpoint and exact request model/user
// lease entry. It does not create files yet; actual AG activation is required.
func (g *GeminiCapture) Begin(ctx context.Context, requestPath string, inboundBody []byte, metadataUserID, requestModel string, stream bool, userID int64) *GeminiCaptureRequest {
	if g == nil || strings.TrimSpace(g.leasePath) == "" || requestPath != "/v1/messages" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lease, expiresAt, ok := readGeminiCaptureLease(g.leasePath)
	if !ok || !lease.Enabled || lease.Model != GeminiCaptureTargetModel || requestModel != GeminiCaptureTargetModel || !containsExact(lease.MetadataUserIDs, metadataUserID) {
		return nil
	}
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	clientRequestID, _ := ctx.Value(ctxkey.ClientRequestID).(string)
	captureLen := len(inboundBody)
	truncated := false
	if captureLen > geminiCaptureMaxBytes {
		captureLen = geminiCaptureMaxBytes
		truncated = true
	}
	capturedBody := make([]byte, captureLen)
	copy(capturedBody, inboundBody[:captureLen])
	redacted, fields, redactionOK := redactGeminiCaptureJSON(capturedBody)
	return &GeminiCaptureRequest{
		controller:         g,
		ctx:                ctx,
		candidate:          true,
		captureEnabled:     true,
		incompleteReason:   make(map[string]struct{}),
		terminals:          make(map[string]struct{}),
		metadataUserID:     metadataUserID,
		requestModel:       requestModel,
		stream:             stream,
		userID:             userID,
		requestID:          strings.TrimSpace(requestID),
		clientRequestID:    strings.TrimSpace(clientRequestID),
		startedAtTime:      time.Now().UTC(),
		inboundRedacted:    redacted,
		inboundRedactionOK: redactionOK,
		redactedFields:     fields,
		inboundTruncated:   truncated,
		lease:              lease,
		leaseExpiresAt:     expiresAt,
		lastLeaseCheck:     time.Now(),
	}
}

// WithGeminiCapture carries a request-scoped candidate through service calls.
func WithGeminiCapture(ctx context.Context, capture *GeminiCaptureRequest) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if capture == nil {
		return ctx
	}
	return context.WithValue(ctx, geminiCaptureContextKey{}, capture)
}

// GeminiCaptureFromContext retrieves the request-scoped capture, if any.
func GeminiCaptureFromContext(ctx context.Context) *GeminiCaptureRequest {
	if ctx == nil {
		return nil
	}
	capture, _ := ctx.Value(geminiCaptureContextKey{}).(*GeminiCaptureRequest)
	return capture
}

type geminiCaptureContextKey struct{}

// SetGroupID associates the authenticated non-secret group identifier.
func (r *GeminiCaptureRequest) SetGroupID(groupID int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.groupID = groupID
	r.mu.Unlock()
}

// MarkAntigravitySelected preserves a local gateway failure artifact even when
// the selected account fails before any HTTP attempt is constructed.
func (r *GeminiCaptureRequest) MarkAntigravitySelected(accountID int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.selectedAG = true
	if accountID > 0 {
		r.selectedAccountID = accountID
	}
	r.mu.Unlock()
}

// Activate records the transformed Gemini body after the final model is known.
// It is intentionally callable only by the Antigravity Messages forwarding path.
func (r *GeminiCaptureRequest) Activate(account *Account, finalModel string, geminiBody []byte) bool {
	if r == nil || account == nil || account.Platform != PlatformAntigravity || account.Type == AccountTypeAPIKey || account.Type == AccountTypeUpstream {
		return false
	}
	modelMatched := finalModel == GeminiCaptureTargetModel
	r.mu.Lock()
	r.modelChecked = true
	r.modelMatched = modelMatched
	r.selectedAG = true
	r.mu.Unlock()
	if !modelMatched {
		return false
	}
	return r.activateArtifact(geminiBody)
}

func (r *GeminiCaptureRequest) activateArtifact(geminiBody []byte) bool {
	r.mu.Lock()
	if r.finalized || r.activated || !r.captureEnabled {
		r.mu.Unlock()
		return r.activated
	}
	r.mu.Unlock()

	lease, expiresAt, ok := readGeminiCaptureLease(r.controller.leasePath)
	if !ok || !lease.Enabled || lease.Model != GeminiCaptureTargetModel || !containsExact(lease.MetadataUserIDs, r.metadataUserID) || !privateOutputDir(lease.OutputDir) {
		r.disable("lease_invalid_or_expired")
		return false
	}
	if time.Now().After(expiresAt) {
		r.disable("lease_expired")
		return false
	}
	budget := r.controller.outputBudget(lease.OutputDir)
	if !budget.readyForCapture() || !budget.reserve(geminiCaptureManifestReserve) {
		r.disable("shared_budget_unready")
		return false
	}
	r.mu.Lock()
	r.budgetReservation = geminiCaptureManifestReserve
	r.budget = budget
	r.mu.Unlock()
	q := newGeminiCaptureQueue(budget)
	if !q.writerAdmitted {
		q.close()
		r.releaseBudgetReservation()
		r.disable("writer_admission")
		return false
	}
	artifactDir := filepath.Join(lease.OutputDir, "gemini-"+uuid.NewString())
	if err := os.Mkdir(artifactDir, 0700); err != nil {
		q.close()
		r.releaseBudgetReservation()
		r.disable("artifact_dir_unavailable")
		return false
	}
	if err := os.Chmod(artifactDir, 0700); err != nil || !privateOutputDir(artifactDir) {
		q.close()
		_ = os.RemoveAll(artifactDir)
		r.releaseBudgetReservation()
		r.disable("artifact_dir_not_private")
		return false
	}
	openFile := func(target, rel string) bool {
		path := filepath.Join(artifactDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			q.markIncomplete("file_parent_unavailable")
			return false
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			q.markIncomplete("file_open_failed")
			return false
		}
		_ = os.Chmod(path, 0600)
		q.addFile(target, rel, f)
		return true
	}
	if !openFile("inbound", "inbound.bin") || !openFile("gemini_request", "gemini_request.bin") || !openFile("converted", "converted.bin") {
		q.close()
		_ = os.RemoveAll(artifactDir)
		r.disable("file_open_failed")
		return false
	}

	r.mu.Lock()
	if r.finalized || !r.captureEnabled {
		r.mu.Unlock()
		q.close()
		_ = os.RemoveAll(artifactDir)
		return false
	}
	r.lease = lease
	r.leaseExpiresAt = expiresAt
	r.lastLeaseCheck = time.Now()
	r.artifactDir = artifactDir
	r.queue = q
	r.activated = true
	r.captureEnabled = true
	r.mu.Unlock()

	if r.inboundTruncated {
		r.addIncomplete("inbound_quota")
	}
	if !r.inboundRedactionOK {
		r.addIncomplete("inbound_redaction_parse_failed")
		q.markIncomplete("inbound_redaction_parse_failed")
	} else {
		r.mu.Lock()
		inboundRedacted := len(r.redactedFields) > 0
		r.mu.Unlock()
		if inboundRedacted {
			r.addIncomplete("credential_redaction_applied")
			q.markFileIncomplete("inbound", "credential_redaction_applied")
		}
		if !q.enqueue("inbound", r.inboundRedacted, len(r.inboundRedacted)) {
			r.addIncomplete("inbound_write_failed")
		}
	}
	redactedGemini, fields, redactionOK := redactGeminiCaptureJSON(geminiBody)
	r.mu.Lock()
	r.redactedFields = appendUniqueStrings(r.redactedFields, fields...)
	r.mu.Unlock()
	if !redactionOK {
		r.addIncomplete("gemini_request_redaction_parse_failed")
		q.markIncomplete("gemini_request_redaction_parse_failed")
	} else {
		if len(fields) > 0 {
			r.addIncomplete("credential_redaction_applied")
			q.markFileIncomplete("gemini_request", "credential_redaction_applied")
		}
		if !q.enqueue("gemini_request", redactedGemini, len(redactedGemini)) {
			r.addIncomplete("gemini_request_write_failed")
		}
	}
	return true
}

// BeginUpstreamAttempt registers one actual upstream HTTP attempt and stores its
// exact (credential-redacted) request body. It is safe to call from retry code.
func (r *GeminiCaptureRequest) BeginUpstreamAttempt(accountID, groupID int64, body []byte) *GeminiCaptureAttempt {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if !r.activated || r.finalized || !r.captureEnabled || r.queue == nil {
		r.mu.Unlock()
		return nil
	}
	if groupID == 0 {
		groupID = r.groupID
	}
	r.nextSeq++
	seq := r.nextSeq
	artifactDir := r.artifactDir
	q := r.queue
	r.mu.Unlock()

	requestRel := fmt.Sprintf("attempts/%03d/request.bin", seq)
	responseRel := fmt.Sprintf("attempts/%03d/upstream.bin", seq)
	// Use stable logical keys rather than path-derived keys so manifest
	// assembly remains independent of platform path separators.
	requestTarget := fmt.Sprintf("attempt-%03d-request", seq)
	responseTarget := fmt.Sprintf("attempt-%03d-response", seq)
	requestPath := filepath.Join(artifactDir, filepath.FromSlash(requestRel))
	responsePath := filepath.Join(artifactDir, filepath.FromSlash(responseRel))
	if err := os.MkdirAll(filepath.Dir(requestPath), 0700); err != nil {
		q.markIncomplete("attempt_parent_unavailable")
		return nil
	}
	requestFile, err := os.OpenFile(requestPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		q.markIncomplete("attempt_request_open_failed")
		return nil
	}
	responseFile, err := os.OpenFile(responsePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		_ = requestFile.Close()
		q.markIncomplete("attempt_response_open_failed")
		return nil
	}
	_ = os.Chmod(requestPath, 0600)
	_ = os.Chmod(responsePath, 0600)
	q.addFile(requestTarget, requestRel, requestFile)
	q.addFile(responseTarget, responseRel, responseFile)

	attempt := &GeminiCaptureAttempt{
		trace: r, sequence: seq, accountID: accountID, groupID: groupID,
		requestFile: requestTarget, responseFile: responseTarget,
	}
	redacted, fields, redactionOK := redactGeminiCaptureJSON(body)
	r.mu.Lock()
	r.redactedFields = appendUniqueStrings(r.redactedFields, fields...)
	r.attempts = append(r.attempts, attempt)
	r.mu.Unlock()
	if !redactionOK {
		r.addIncomplete("attempt_request_redaction_parse_failed")
		q.markFileIncomplete(requestTarget, "attempt_request_redaction_parse_failed")
	} else {
		if len(fields) > 0 {
			r.addIncomplete("credential_redaction_applied")
			q.markFileIncomplete(requestTarget, "credential_redaction_applied")
		}
		if !q.enqueue(requestTarget, redacted, len(redacted)) {
			r.addIncomplete("attempt_request_write_failed")
		}
	}
	return attempt
}

// AttachResponse wraps the upstream body before Scanner or error readers see it.
func (a *GeminiCaptureAttempt) AttachResponse(resp *http.Response) *http.Response {
	if a == nil || resp == nil {
		return resp
	}
	a.mu.Lock()
	a.statusCode = resp.StatusCode
	a.upstreamRequestID = strings.TrimSpace(resp.Header.Get("x-request-id"))
	a.mu.Unlock()
	if resp.Body != nil {
		resp.Body = &geminiCaptureReadCloser{ReadCloser: resp.Body, attempt: a}
	}
	return resp
}

func (a *GeminiCaptureAttempt) MarkRequestError(err error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if err != nil {
		a.requestError = sanitizeCaptureError(err.Error())
	}
	a.mu.Unlock()
}

func (a *GeminiCaptureAttempt) markRead(n int, err error, p []byte) {
	if a == nil {
		return
	}
	if n > 0 {
		a.trace.appendAttemptBytes(a, p[:n])
	}
	if err == io.EOF {
		a.mu.Lock()
		a.responseEOF = true
		a.mu.Unlock()
		a.trace.mu.Lock()
		a.trace.upstreamEOF = true
		a.trace.mu.Unlock()
		a.trace.markTermination("eof")
	} else if err != nil {
		a.mu.Lock()
		a.responseReadError = true
		a.mu.Unlock()
		a.trace.mu.Lock()
		a.trace.upstreamReadError = true
		a.trace.mu.Unlock()
		a.trace.markTermination("read_error")
		if errors.Is(err, context.Canceled) {
			a.MarkContextCanceled()
		} else if errors.Is(err, context.DeadlineExceeded) {
			a.MarkTimeout()
		}
	}
}

func (a *GeminiCaptureAttempt) markClosed() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if !a.responseEOF {
		a.responseClosed = true
		a.responseReadError = true
	}
	a.mu.Unlock()
	if !a.responseEOF {
		a.trace.mu.Lock()
		a.trace.upstreamReadError = true
		a.trace.mu.Unlock()
		a.trace.markTermination("read_error")
		a.trace.addIncomplete("upstream_response_closed_before_eof")
	}
}

func (a *GeminiCaptureAttempt) MarkClientDisconnect() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.clientDisconnect = true
	a.mu.Unlock()
	a.trace.mu.Lock()
	a.trace.clientDisconnect = true
	a.trace.mu.Unlock()
	a.trace.markTermination("client_disconnect")
}

func (a *GeminiCaptureAttempt) MarkContextCanceled() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.ctxCanceled = true
	a.mu.Unlock()
	a.trace.mu.Lock()
	a.trace.ctxCanceled = true
	a.trace.mu.Unlock()
	a.trace.markTermination("ctx_cancel")
}

func (a *GeminiCaptureAttempt) MarkTimeout() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.timeout = true
	a.mu.Unlock()
	a.trace.mu.Lock()
	a.trace.timeout = true
	a.trace.mu.Unlock()
	a.trace.markTermination("timeout")
}

func (r *GeminiCaptureRequest) appendAttemptBytes(a *GeminiCaptureAttempt, p []byte) {
	if r == nil || a == nil || len(p) == 0 {
		return
	}
	if !r.leaseStillValid() {
		return
	}
	if !r.appendSanitizedFile(a.responseFile, p) {
		r.addIncomplete("upstream_response_write_failed")
	}
}

func (r *GeminiCaptureRequest) appendSanitizedFile(target string, p []byte) bool {
	if r == nil || len(p) == 0 {
		return true
	}
	r.mu.Lock()
	if r.sanitizers == nil {
		r.sanitizers = make(map[string]*geminiCaptureStreamSanitizer)
	}
	sanitizer := r.sanitizers[target]
	if sanitizer == nil {
		sanitizer = &geminiCaptureStreamSanitizer{trace: r, target: target}
		r.sanitizers[target] = sanitizer
	}
	r.mu.Unlock()
	return sanitizer.append(p)
}

func (r *GeminiCaptureRequest) flushCaptureFile(target string) {
	r.mu.Lock()
	sanitizer := r.sanitizers[target]
	r.mu.Unlock()
	if sanitizer != nil {
		sanitizer.flush()
	}
}

func (r *GeminiCaptureRequest) flushAllCaptureFiles() {
	r.mu.Lock()
	sanitizers := make([]*geminiCaptureStreamSanitizer, 0, len(r.sanitizers))
	for _, sanitizer := range r.sanitizers {
		sanitizers = append(sanitizers, sanitizer)
	}
	r.mu.Unlock()
	for _, sanitizer := range sanitizers {
		sanitizer.flush()
	}
}

func (s *geminiCaptureStreamSanitizer) append(p []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled {
		return false
	}
	if len(s.pending)+len(p) > geminiCaptureParserMaxBytes {
		s.disabled = true
		s.pending = nil
		s.trace.markCaptureFileIncomplete(s.target, "parser_quota")
		return false
	}
	s.pending = append(s.pending, p...)
	for {
		idx := bytes.IndexByte(s.pending, '\n')
		if idx < 0 {
			break
		}
		line := append([]byte(nil), s.pending[:idx+1]...)
		s.pending = s.pending[idx+1:]
		if !s.emit(line) {
			s.disabled = true
			s.pending = nil
			return false
		}
	}
	return true
}

func (s *geminiCaptureStreamSanitizer) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled || len(s.pending) == 0 {
		return
	}
	line := append([]byte(nil), s.pending...)
	s.pending = nil
	if !s.emit(line) {
		s.disabled = true
	}
}

func (s *geminiCaptureStreamSanitizer) emit(line []byte) bool {
	redacted, fields, changed, safe := redactGeminiCaptureLine(line)
	if !safe {
		s.trace.markCaptureFileIncomplete(s.target, "credential_redaction_parse_failed")
		return false
	}
	if changed {
		s.trace.addRedactedFields(fields...)
		reason := "credential_redaction_applied"
		parseFailed := false
		for _, field := range fields {
			if field == "[redaction_parse_failed]" {
				reason = "credential_redaction_parse_failed"
				parseFailed = true
				break
			}
		}
		s.trace.markCaptureFileIncomplete(s.target, reason)
		if parseFailed {
			s.disabled = true
		}
	}
	s.trace.mu.Lock()
	q := s.trace.queue
	s.trace.mu.Unlock()
	return q != nil && q.enqueue(s.target, redacted, len(redacted))
}

func (r *GeminiCaptureRequest) reserveManifestBytes(size int64) bool {
	if size <= 0 {
		return true
	}
	r.mu.Lock()
	budget := r.budget
	reserved := r.budgetReservation
	r.mu.Unlock()
	if budget == nil {
		return false
	}
	if size <= reserved {
		return true
	}
	delta := size - reserved
	if !budget.reserve(delta) {
		return false
	}
	r.mu.Lock()
	r.budgetReservation = size
	r.mu.Unlock()
	return true
}

func (r *GeminiCaptureRequest) releaseBudgetReservation() {
	if r == nil {
		return
	}
	r.mu.Lock()
	budget := r.budget
	amount := r.budgetReservation
	r.budgetReservation = 0
	r.mu.Unlock()
	if budget != nil && amount > 0 {
		budget.release(amount)
	}
}

func (r *GeminiCaptureRequest) addRedactedFields(fields ...string) {
	if len(fields) == 0 {
		return
	}
	r.mu.Lock()
	r.redactedFields = appendUniqueStrings(r.redactedFields, fields...)
	r.mu.Unlock()
}

func (r *GeminiCaptureRequest) markCaptureFileIncomplete(target, reason string) {
	r.addIncomplete(reason)
	r.mu.Lock()
	q := r.queue
	r.mu.Unlock()
	if q != nil {
		q.markFileIncomplete(target, reason)
	}
}

func redactGeminiCaptureLine(line []byte) ([]byte, []string, bool, bool) {
	ending := []byte{}
	body := line
	if bytes.HasSuffix(body, []byte("\r\n")) {
		ending = []byte("\r\n")
		body = body[:len(body)-2]
	} else if len(body) > 0 && (body[len(body)-1] == '\n' || body[len(body)-1] == '\r') {
		ending = body[len(body)-1:]
		body = body[:len(body)-1]
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || strings.HasPrefix(trimmed, ":") || trimmed == "[DONE]" || strings.HasPrefix(trimmed, "event:") || strings.HasPrefix(trimmed, "id:") || strings.HasPrefix(trimmed, "retry:") {
		return append([]byte(nil), line...), nil, false, true
	}
	if strings.HasPrefix(trimmed, "data:") {
		leading := len(body) - len(strings.TrimLeftFunc(string(body), unicode.IsSpace))
		dataStart := leading + len("data:")
		rawPayload := body[dataStart:]
		left := len(rawPayload) - len(strings.TrimLeftFunc(string(rawPayload), unicode.IsSpace))
		trailing := len(rawPayload) - len(strings.TrimRightFunc(string(rawPayload), unicode.IsSpace))
		payloadEnd := len(rawPayload) - trailing
		if payloadEnd <= left {
			return append([]byte(nil), line...), nil, false, true
		}
		payload := rawPayload[left:payloadEnd]
		if strings.TrimSpace(string(payload)) == "[DONE]" {
			return append([]byte(nil), line...), nil, false, true
		}
		redacted, fields, ok := redactGeminiCaptureJSON(payload)
		if !ok {
			return line, nil, false, false
		}
		if len(fields) == 0 {
			return append([]byte(nil), line...), nil, false, true
		}
		out := make([]byte, 0, len(body)+len(redacted)-len(payload)+len(ending))
		out = append(out, body[:dataStart+left]...)
		out = append(out, redacted...)
		out = append(out, rawPayload[payloadEnd:]...)
		out = append(out, ending...)
		return out, fields, true, true
	}
	redacted, fields, ok := redactGeminiCaptureJSON(body)
	if !ok {
		return line, nil, false, false
	}
	if len(fields) == 0 {
		return append([]byte(nil), line...), nil, false, true
	}
	return append(redacted, ending...), fields, true, true
}

func (r *GeminiCaptureRequest) RecordConvertedWrite(p []byte, n int, err error) {
	if r == nil {
		return
	}
	if len(p) == 0 {
		if err != nil || n != 0 {
			r.addIncomplete("client_write_partial_or_failed")
		}
		return
	}
	if n < 0 {
		n = 0
	}
	if n > len(p) {
		n = len(p)
	}
	if !r.leaseStillValid() {
		return
	}
	r.mu.Lock()
	r.convertedWritten = r.convertedWritten || n > 0
	r.mu.Unlock()
	if !r.appendSanitizedFile("converted", p[:n]) {
		r.addIncomplete("converted_write_failed")
	}
	if err != nil || n != len(p) {
		r.addIncomplete("client_write_partial_or_failed")
	}
}

// RecordConvertedBody records non-streaming output before c.Data writes it.
func (r *GeminiCaptureRequest) RecordConvertedBody(body []byte) {
	r.RecordConvertedWrite(body, len(body), nil)
}

func (r *GeminiCaptureRequest) MarkTimeout() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.timeout = true
	r.mu.Unlock()
	r.markTermination("timeout")
}

func (r *GeminiCaptureRequest) MarkContextCanceled() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.ctxCanceled = true
	r.mu.Unlock()
	r.markTermination("ctx_cancel")
}

func (r *GeminiCaptureRequest) MarkClientDisconnect() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.clientDisconnect = true
	r.mu.Unlock()
	r.markTermination("client_disconnect")
}

func (r *GeminiCaptureRequest) markTermination(reason string) {
	r.mu.Lock()
	if r.terminals == nil {
		r.terminals = make(map[string]struct{})
	}
	r.terminals[reason] = struct{}{}
	r.mu.Unlock()
}

func (r *GeminiCaptureRequest) addIncomplete(reason string) {
	if r == nil || reason == "" {
		return
	}
	r.mu.Lock()
	if r.incompleteReason == nil {
		r.incompleteReason = make(map[string]struct{})
	}
	r.incompleteReason[reason] = struct{}{}
	r.mu.Unlock()
}

func (r *GeminiCaptureRequest) disable(reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.captureEnabled = false
	if reason != "" {
		if r.incompleteReason == nil {
			r.incompleteReason = make(map[string]struct{})
		}
		r.incompleteReason[reason] = struct{}{}
	}
	r.mu.Unlock()
}

func (r *GeminiCaptureRequest) leaseStillValid() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if !r.captureEnabled || r.finalized || r.queue == nil {
		r.mu.Unlock()
		return false
	}
	now := time.Now()
	if !now.Before(r.leaseExpiresAt) {
		r.captureEnabled = false
		r.incompleteReason["lease_expired"] = struct{}{}
		r.mu.Unlock()
		return false
	}
	if time.Since(r.lastLeaseCheck) < geminiCaptureLeasePoll || r.leaseRefreshInFlight {
		r.mu.Unlock()
		return true
	}
	path := r.controller.leasePath
	metadataUserID := r.metadataUserID
	r.lastLeaseCheck = now
	r.leaseRefreshInFlight = true
	r.mu.Unlock()

	// Do not make the stream writer wait on filesystem I/O. One bounded
	// refresh is allowed in flight; it either renews the snapshot or disables
	// subsequent appends. Finish performs the final manifest after queued data.
	go func() {
		lease, expiresAt, ok := readGeminiCaptureLease(path)
		valid := ok && lease.Enabled && lease.Model == GeminiCaptureTargetModel && containsExact(lease.MetadataUserIDs, metadataUserID) && time.Now().Before(expiresAt)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.leaseRefreshInFlight = false
		if r.finalized || !r.captureEnabled {
			return
		}
		if !valid {
			r.captureEnabled = false
			r.incompleteReason["lease_expired_or_disarmed"] = struct{}{}
			return
		}
		r.lease = lease
		r.leaseExpiresAt = expiresAt
	}()
	return true
}

// Finish is called by the handler defer. It never returns an error to the
// gateway, and all write/permission failures are represented as incomplete.
func (r *GeminiCaptureRequest) Finish(status int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.finalized {
		r.mu.Unlock()
		return
	}
	r.gatewayStatus = status
	needLocalArtifact := r.candidate && !r.activated && (r.selectedAG || status >= 400) && r.captureEnabled && !r.modelChecked
	activated := r.activated
	r.mu.Unlock()

	if !activated && needLocalArtifact && !r.localFailureWorker {
		r.mu.Lock()
		r.finalized = true
		r.mu.Unlock()
		if !enqueueGeminiCaptureFailure(r) {
			r.addIncomplete("local_failure_queue_full")
		}
		return
	}
	if !activated && needLocalArtifact {
		if r.activateLocalFailureArtifact() {
			activated = true
		}
	}
	if !activated {
		r.releaseBudgetReservation()
		r.mu.Lock()
		r.finalized = true
		r.mu.Unlock()
		return
	}

	if activated {
		r.flushAllCaptureFiles()
	}
	r.mu.Lock()
	q := r.queue
	r.finalized = true
	if r.ctx != nil && errors.Is(r.ctx.Err(), context.Canceled) {
		r.ctxCanceled = true
		r.terminals["ctx_cancel"] = struct{}{}
	}
	r.mu.Unlock()
	if q == nil {
		return
	}
	q.close()
	files, queueReasons := q.snapshot()
	r.mu.Lock()
	for _, reason := range queueReasons {
		r.incompleteReason[reason] = struct{}{}
	}
	manifest := r.buildManifestLocked(files)
	artifactDir := r.artifactDir
	r.mu.Unlock()
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		r.addIncomplete("manifest_marshal_failed")
		return
	}
	manifestBody := append(manifestBytes, '\n')
	if !r.reserveManifestBytes(int64(len(manifestBody))) {
		r.addIncomplete("manifest_budget")
		return
	}
	manifestPath := filepath.Join(artifactDir, "manifest.json")
	if err := writeGeminiCaptureManifestBounded(manifestPath, manifestBody); err != nil {
		r.addIncomplete("manifest_write_failed")
		return
	}
}

func startGeminiCaptureManifestWorkers() {
	geminiCaptureManifestOnce.Do(func() {
		geminiCaptureManifestQueue = make(chan *geminiCaptureManifestTask, geminiCaptureManifestQueueSize)
		for i := 0; i < geminiCaptureAbortWorkerCount; i++ {
			go func() {
				for task := range geminiCaptureManifestQueue {
					task.done <- writeGeminiCaptureManifestTask(task)
				}
			}()
		}
	})
}

func writeGeminiCaptureManifestTask(task *geminiCaptureManifestTask) error {
	isCancelled := func() bool {
		select {
		case <-task.cancelled:
			return true
		default:
			return false
		}
	}
	if isCancelled() {
		return errors.New("manifest write cancelled")
	}
	tmpPath := task.path + ".tmp"
	if err := os.WriteFile(tmpPath, task.body, 0600); err != nil {
		return err
	}
	_ = os.Chmod(tmpPath, 0600)
	if isCancelled() {
		_ = os.Remove(tmpPath)
		return errors.New("manifest write cancelled")
	}
	if err := os.Rename(tmpPath, task.path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if isCancelled() {
		_ = os.Remove(task.path)
		return errors.New("manifest write cancelled")
	}
	_ = os.Chmod(task.path, 0600)
	return nil
}

func writeGeminiCaptureManifestBounded(path string, body []byte) error {
	startGeminiCaptureManifestWorkers()
	task := &geminiCaptureManifestTask{path: path, body: body, cancelled: make(chan struct{}), done: make(chan error, 1)}
	select {
	case geminiCaptureManifestQueue <- task:
	default:
		return errors.New("manifest queue full")
	}
	timer := time.NewTimer(geminiCaptureManifestTimeout)
	defer timer.Stop()
	select {
	case err := <-task.done:
		return err
	case <-timer.C:
		task.cancelOnce.Do(func() { close(task.cancelled) })
		return errors.New("manifest write timeout")
	}
}

func (r *GeminiCaptureRequest) activateLocalFailureArtifact() bool {
	r.mu.Lock()
	lease := r.lease
	expiresAt := r.leaseExpiresAt
	r.mu.Unlock()
	if !lease.Enabled || lease.Model != GeminiCaptureTargetModel || !containsExact(lease.MetadataUserIDs, r.metadataUserID) || time.Now().After(expiresAt) || !privateOutputDir(lease.OutputDir) {
		r.disable("lease_invalid_or_expired")
		return false
	}
	budget := r.controller.outputBudget(lease.OutputDir)
	if !budget.readyForCapture() || !budget.reserve(geminiCaptureManifestReserve) {
		r.disable("shared_budget_unready")
		return false
	}
	r.mu.Lock()
	r.budgetReservation = geminiCaptureManifestReserve
	r.budget = budget
	r.mu.Unlock()
	q := newGeminiCaptureQueue(budget)
	if !q.writerAdmitted {
		q.close()
		r.releaseBudgetReservation()
		r.disable("writer_admission")
		return false
	}
	artifactDir := filepath.Join(lease.OutputDir, "gemini-"+uuid.NewString())
	if err := os.Mkdir(artifactDir, 0700); err != nil || !privateOutputDir(artifactDir) {
		q.close()
		r.releaseBudgetReservation()
		r.disable("artifact_dir_unavailable")
		return false
	}
	openFile := func(target, rel string) bool {
		path := filepath.Join(artifactDir, filepath.FromSlash(rel))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			q.markIncomplete("file_open_failed")
			return false
		}
		_ = os.Chmod(path, 0600)
		q.addFile(target, rel, f)
		return true
	}
	if !openFile("inbound", "inbound.bin") {
		q.close()
		_ = os.RemoveAll(artifactDir)
		return false
	}
	r.mu.Lock()
	if r.finalized || !r.captureEnabled {
		r.mu.Unlock()
		q.close()
		_ = os.RemoveAll(artifactDir)
		return false
	}
	r.lease = lease
	r.leaseExpiresAt = expiresAt
	r.artifactDir = artifactDir
	r.queue = q
	r.activated = true
	r.mu.Unlock()
	if !q.enqueue("inbound", r.inboundRedacted, len(r.inboundRedacted)) {
		r.addIncomplete("inbound_write_failed")
	}
	return true
}

func (r *GeminiCaptureRequest) buildManifestLocked(files map[string]GeminiCaptureFileEvidence) geminiCaptureManifest {
	attempts := make([]geminiCaptureAttemptEvidence, 0, len(r.attempts))
	for _, attempt := range r.attempts {
		attempt.mu.Lock()
		attemptOutcome := "http_response"
		if attempt.requestError != "" {
			attemptOutcome = "request_error"
		} else if attempt.timeout {
			attemptOutcome = "timeout"
		} else if attempt.ctxCanceled {
			attemptOutcome = "ctx_cancel"
		} else if attempt.clientDisconnect {
			attemptOutcome = "client_disconnect"
		} else if attempt.responseReadError {
			attemptOutcome = "read_error"
		} else if attempt.responseEOF {
			attemptOutcome = "eof"
		}
		evidence := geminiCaptureAttemptEvidence{
			Sequence: attempt.sequence, AccountID: attempt.accountID, GroupID: attempt.groupID,
			StatusCode: attempt.statusCode, Outcome: attemptOutcome, UpstreamRequestID: attempt.upstreamRequestID,
			ResponseEOF: attempt.responseEOF, ResponseComplete: attempt.responseEOF && !attempt.responseReadError,
			ResponseReadError: attempt.responseReadError, RequestError: attempt.requestError,
			ClientDisconnect: attempt.clientDisconnect, ContextCanceled: attempt.ctxCanceled, Timeout: attempt.timeout,
		}
		if f, ok := files[attempt.requestFile]; ok {
			evidence.RequestFile = f.Path
		}
		if f, ok := files[attempt.responseFile]; ok {
			evidence.ResponseFile = f.Path
			if !f.Complete {
				evidence.ResponseComplete = false
			}
		}
		attempt.mu.Unlock()
		attempts = append(attempts, evidence)
	}
	termination := make([]string, 0, len(r.terminals))
	for reason := range r.terminals {
		termination = append(termination, reason)
	}
	sort.Strings(termination)
	incompleteReasons := make([]string, 0, len(r.incompleteReason))
	for reason := range r.incompleteReason {
		incompleteReasons = append(incompleteReasons, reason)
	}
	sort.Strings(incompleteReasons)
	if r.inboundTruncated {
		incompleteReasons = appendUniqueStrings(incompleteReasons, "inbound_quota")
	}
	sort.Strings(incompleteReasons)
	incomplete := len(incompleteReasons) > 0
	for _, file := range files {
		if !file.Complete {
			incomplete = true
			break
		}
	}
	outcome := "completed"
	switch {
	case r.gatewayStatus >= 400 && len(attempts) == 0:
		outcome = "gateway_error_no_upstream_attempt"
	case r.timeout:
		outcome = "timeout"
	case r.clientDisconnect:
		outcome = "client_disconnect"
	case r.ctxCanceled:
		outcome = "ctx_cancel"
	case r.upstreamReadError:
		outcome = "upstream_read_error"
	case r.upstreamEOF:
		outcome = "upstream_eof"
	case len(attempts) > 0 && r.gatewayStatus >= 400:
		outcome = "upstream_http_error"
	}
	return geminiCaptureManifest{
		SchemaVersion: geminiCaptureSchemaVersion, StartedAt: r.startedAt(), FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
		RequestID: r.requestID, ClientRequestID: r.clientRequestID, UserID: r.userID,
		GroupID: r.groupID, SelectedAccountID: r.selectedAccountID,
		MetadataUserID: r.metadataUserID, RequestPath: "/v1/messages", RequestModel: r.requestModel,
		FinalModel: r.finalModelLocked(), Stream: r.stream, AGChain: r.selectedAG,
		GatewayStatus: r.gatewayStatus, Outcome: outcome, UpstreamAttempts: len(attempts),
		UpstreamFinishReasons: r.collectFinishReasons(files), Usage: r.usage, Termination: termination,
		Incomplete: incomplete, IncompleteReasons: incompleteReasons, RedactedFields: r.redactedFields,
		HeadersRecorded: false, CredentialHandling: "JSON credential-like fields are replaced with [REDACTED]; HTTP headers are never recorded; thoughtSignature is retained.",
		Files: files, Attempts: attempts,
	}
}

func (r *GeminiCaptureRequest) startedAt() string {
	if r.startedAtTime.IsZero() {
		return ""
	}
	return r.startedAtTime.Format(time.RFC3339Nano)
}

func (r *GeminiCaptureRequest) finalModelLocked() string {
	if r.modelMatched {
		return GeminiCaptureTargetModel
	}
	return ""
}

func (r *GeminiCaptureRequest) collectFinishReasons(files map[string]GeminiCaptureFileEvidence) []string {
	// Finish reasons are accumulated by the raw SSE parser below. The parser
	// stores them as synthetic terminal keys, avoiding any body re-read. The
	// caller already holds r.mu while building the manifest.
	result := make([]string, 0)
	for key := range r.terminals {
		if strings.HasPrefix(key, "finish:") {
			result = append(result, strings.TrimPrefix(key, "finish:"))
		}
	}
	sort.Strings(result)
	_ = files
	return result
}

func (r *GeminiCaptureRequest) parseUpstreamChunk(p []byte) {
	if r == nil || len(p) == 0 {
		return
	}
	// Scanner strips line endings, so this parser receives the original reader
	// chunks and keeps a carry buffer to avoid losing a JSON line split across
	// network reads. The exact bytes remain in upstream.bin unchanged.
	r.mu.Lock()
	if r.parserDisabled {
		r.mu.Unlock()
		return
	}
	if len(r.parseBuffer)+len(p) > geminiCaptureParserMaxBytes {
		r.parserDisabled = true
		r.parseBuffer = ""
		r.incompleteReason["parser_quota"] = struct{}{}
		r.mu.Unlock()
		return
	}
	combined := r.parseBuffer + string(p)
	lines := strings.Split(strings.ReplaceAll(combined, "\r\n", "\n"), "\n")
	r.parseBuffer = lines[len(lines)-1]
	r.mu.Unlock()
	for _, line := range lines[:len(lines)-1] {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var value any
		if json.Unmarshal([]byte(payload), &value) != nil {
			continue
		}
		r.observeCapturePayload(value)
	}
}

func (r *GeminiCaptureRequest) flushCaptureParser() {
	if r == nil {
		return
	}
	r.mu.Lock()
	line := r.parseBuffer
	r.parseBuffer = ""
	r.mu.Unlock()
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var value any
	if json.Unmarshal([]byte(payload), &value) == nil {
		r.observeCapturePayload(value)
	}
}

func (r *GeminiCaptureRequest) observeCapturePayload(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if usage, ok := typed["usageMetadata"].(map[string]any); ok {
			r.mu.Lock()
			r.usage = cloneCaptureMap(usage)
			r.mu.Unlock()
		}
		if reason, ok := typed["finishReason"].(string); ok && reason != "" {
			r.markTermination("finish:" + reason)
		}
		for _, child := range typed {
			r.observeCapturePayload(child)
		}
	case []any:
		for _, child := range typed {
			r.observeCapturePayload(child)
		}
	}
}

func cloneCaptureMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, child := range value {
		result[key] = child
	}
	return result
}

type geminiCaptureReadCloser struct {
	io.ReadCloser
	attempt *GeminiCaptureAttempt
}

func (r *geminiCaptureReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if r.attempt != nil {
		r.attempt.markRead(n, err, p)
		if n > 0 {
			r.attempt.trace.parseUpstreamChunk(p[:n])
		}
		if err == io.EOF {
			r.attempt.trace.flushCaptureParser()
			r.attempt.trace.flushCaptureFile(r.attempt.responseFile)
		}
	}
	return n, err
}

func (r *geminiCaptureReadCloser) Close() error {
	if r.attempt != nil {
		r.attempt.markClosed()
		r.attempt.trace.flushCaptureParser()
		r.attempt.trace.flushCaptureFile(r.attempt.responseFile)
	}
	return r.ReadCloser.Close()
}

func readGeminiCaptureLease(path string) (GeminiCaptureLease, time.Time, bool) {
	var lease GeminiCaptureLease
	path = strings.TrimSpace(path)
	if path == "" {
		return lease, time.Time{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return lease, time.Time{}, false
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, geminiCaptureLeaseMaxBytes+1))
	if err != nil || len(body) > geminiCaptureLeaseMaxBytes {
		return lease, time.Time{}, false
	}
	if json.Unmarshal(body, &lease) != nil {
		return lease, time.Time{}, false
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(lease.ExpiresAt))
	if err != nil || expiresAt.IsZero() || !expiresAt.After(time.Now()) {
		return lease, time.Time{}, false
	}
	if strings.TrimSpace(lease.Model) != GeminiCaptureTargetModel || strings.TrimSpace(lease.OutputDir) == "" || len(lease.MetadataUserIDs) == 0 {
		return lease, time.Time{}, false
	}
	for _, userID := range lease.MetadataUserIDs {
		if strings.TrimSpace(userID) == "" {
			return lease, time.Time{}, false
		}
	}
	lease.ExpiresAt = expiresAt.UTC().Format(time.RFC3339Nano)
	return lease, expiresAt, true
}

func privateOutputDir(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	lstat, err := os.Lstat(path)
	if err != nil || lstat.Mode()&os.ModeSymlink != 0 {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0077 == 0
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func appendUniqueStrings(values []string, more ...string) []string {
	for _, value := range more {
		if value == "" {
			continue
		}
		found := false
		for _, existing := range values {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			values = append(values, value)
		}
	}
	return values
}

func sanitizeCaptureError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 256 {
		value = value[:256]
	}
	return value
}

func redactGeminiCaptureJSON(body []byte) ([]byte, []string, bool) {
	if len(body) == 0 {
		return nil, nil, true
	}
	var value any
	if json.Unmarshal(body, &value) != nil {
		return nil, nil, false
	}
	fields := make([]string, 0)
	redacted := redactGeminiCaptureValue(value, "", &fields)
	if len(fields) == 0 {
		return append([]byte(nil), body...), nil, true
	}
	result, err := json.Marshal(redacted)
	if err != nil {
		return nil, fields, false
	}
	return result, fields, true
}

func redactGeminiCaptureValue(value any, parent string, fields *[]string) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isCredentialCaptureField(key) && !strings.EqualFold(key, "thoughtSignature") {
				typed[key] = "[REDACTED]"
				*fields = appendUniqueStrings(*fields, parentPath(parent, key))
				continue
			}
			typed[key] = redactGeminiCaptureValue(child, parentPath(parent, key), fields)
		}
	case []any:
		for i, child := range typed {
			typed[i] = redactGeminiCaptureValue(child, fmt.Sprintf("%s[%d]", parent, i), fields)
		}
	}
	return value
}

func parentPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func isCredentialCaptureField(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(lower)
	if normalized == "thoughtsignature" || normalized == "opaque" || normalized == "opaquesignature" || normalized == "providersignature" {
		return false
	}
	for _, exact := range []string{"authorization", "auth", "bearer", "accesstoken", "refreshtoken", "idtoken", "token", "apikey", "apitoken", "clientsecret", "privatekey", "password", "cookie", "credential", "secret"} {
		if normalized == exact || strings.HasSuffix(normalized, exact) && (exact == "token" || exact == "secret") {
			return true
		}
	}
	return false
}
