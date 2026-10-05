package cloud

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brimble/pantech-csi/internal/metadata"
)

const (
	bytesPerGiB      = int64(1 << 30)
	defaultTimeout   = 2 * time.Minute
	operationTimeout = 5 * time.Minute
	operationPoll    = 3 * time.Second
	maxAttempts      = 4
	maxListPages     = 100
	maxPageSize      = 100
)

type Client struct {
	base   *url.URL
	http   *http.Client
	config ClientConfig
	poll   time.Duration
}

type ClientConfig struct {
	BaseURL          string
	APIKey           string
	Region           string
	AvailabilityZone string
	HTTPClient       *http.Client
}

func NewClient(config ClientConfig) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(config.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse Pantech API URL: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("pantech API URL must be absolute")
	}
	if !strings.HasSuffix(strings.TrimRight(base.Path, "/"), "/public/v1") {
		base.Path = strings.TrimRight(base.Path, "/") + "/public/v1"
	}
	if config.APIKey == "" {
		return nil, fmt.Errorf("pantech API key is required")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{base: base, http: client, config: config, poll: operationPoll}, nil
}

func (c *Client) CreateVolume(ctx context.Context, spec VolumeSpec) (*Volume, error) {
	if spec.Type == "" {
		return nil, fmt.Errorf("disk_offering_slug is required: %w", ErrInvalid)
	}
	zone := cmp.Or(spec.AvailabilityZone, c.config.AvailabilityZone)
	var accepted acceptedResponse
	var err error
	if spec.SnapshotID != "" {
		accepted, err = c.write(ctx, http.MethodPost, "/snapshots/"+url.PathEscape(spec.SnapshotID)+"/restore", restoreSnapshotBody{
			Name:             spec.Name,
			DiskOfferingSlug: spec.Type,
			SizeGB:           bytesToGiB(spec.SizeBytes),
		})
	} else {
		accepted, err = c.write(ctx, http.MethodPost, "/volumes", createVolumeBody{
			Name:             spec.Name,
			DiskOfferingSlug: spec.Type,
			SizeGB:           bytesToGiB(spec.SizeBytes),
			Region:           cmp.Or(spec.Region, c.config.Region),
		})
	}
	if err != nil {
		return nil, err
	}
	if accepted.ResourceID == "" {
		return nil, fmt.Errorf("pantech response missing resource_id: %w", ErrInvalid)
	}
	return c.getVolume(ctx, accepted.ResourceID, zone)
}

func (c *Client) GetVolumeByID(ctx context.Context, id string) (*Volume, error) {
	return c.getVolume(ctx, id, c.config.AvailabilityZone)
}

func (c *Client) getVolume(ctx context.Context, id string, fallbackZone string) (*Volume, error) {
	var payload apiVolume
	if err := c.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(id), nil, nil, "", decodeJSON(&payload)); err != nil {
		return nil, err
	}
	if payload.ID == "" {
		return nil, ErrNotFound
	}
	volume := payload.toDomain(fallbackZone)
	return &volume, nil
}

func (c *Client) ListVolumes(ctx context.Context, page Page) ([]Volume, string, error) {
	if page.Name != "" || page.AvailabilityZone != "" {
		volumes, err := c.collectVolumes(ctx, page)
		if err != nil {
			return nil, "", err
		}
		return paginate(volumes, page)
	}
	query := url.Values{}
	query.Set("desired_state", "present")
	if page.Size > 0 {
		query.Set("limit", strconv.Itoa(pageLimit(page.Size)))
	}
	if page.Token != "" {
		query.Set("cursor", page.Token)
	}
	var response pageResponse[apiVolume]
	if err := c.do(ctx, http.MethodGet, "/volumes", query, nil, "", decodeJSON(&response)); err != nil {
		return nil, "", err
	}
	volumes := make([]Volume, 0, len(response.Data))
	for _, item := range response.Data {
		volumes = append(volumes, item.toDomain(c.config.AvailabilityZone))
	}
	return volumes, deref(response.NextCursor), nil
}

func (c *Client) collectVolumes(ctx context.Context, page Page) ([]Volume, error) {
	query := url.Values{}
	query.Set("limit", "100")
	query.Set("desired_state", "present")
	if page.Name != "" {
		query.Set("q", page.Name)
	}
	items, err := collectPages[apiVolume](ctx, c, "/volumes", query)
	if err != nil {
		return nil, err
	}
	fallback := cmp.Or(page.AvailabilityZone, c.config.AvailabilityZone)
	volumes := make([]Volume, 0, len(items))
	for _, item := range items {
		volume := item.toDomain(fallback)
		if page.Name != "" && volume.Name != page.Name {
			continue
		}
		if page.AvailabilityZone != "" && volume.AvailabilityZone != page.AvailabilityZone {
			continue
		}
		volumes = append(volumes, volume)
	}
	slices.SortFunc(volumes, func(left Volume, right Volume) int {
		return cmp.Compare(left.ID, right.ID)
	})
	return volumes, nil
}

func (c *Client) DeleteVolume(ctx context.Context, id string) error {
	_, err := c.write(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(id), nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) ResizeVolume(ctx context.Context, id string, sizeBytes int64, availabilityZone string) (int64, error) {
	current, err := c.GetVolumeByID(ctx, id)
	if err != nil {
		return 0, err
	}
	if availabilityZone != "" && current.AvailabilityZone != "" && current.AvailabilityZone != availabilityZone {
		return 0, ErrNotFound
	}
	if current.Type == "" {
		return 0, fmt.Errorf("disk_offering_slug is required: %w", ErrInvalid)
	}
	if _, err := c.write(ctx, http.MethodPost, "/volumes/"+url.PathEscape(id)+"/resize", resizeVolumeBody{
		DiskOfferingSlug: current.Type,
		SizeGB:           bytesToGiB(sizeBytes),
	}); err != nil {
		return 0, err
	}
	volume, err := c.GetVolumeByID(ctx, id)
	if err != nil {
		return 0, err
	}
	return volume.SizeBytes, nil
}

func (c *Client) AttachVolume(ctx context.Context, volumeID, instanceID string, availabilityZone string) (string, error) {
	if _, err := c.write(ctx, http.MethodPost, "/volumes/"+url.PathEscape(volumeID)+"/attach", attachVolumeBody{
		InstanceID: instanceID,
	}); err != nil {
		return "", err
	}
	return c.waitAttached(ctx, volumeID, instanceID, availabilityZone)
}

func (c *Client) waitAttached(ctx context.Context, volumeID string, instanceID string, availabilityZone string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		volume, err := c.GetVolumeByID(ctx, volumeID)
		if err != nil {
			return "", fmt.Errorf("wait for attached volume %s: %w", volumeID, err)
		}
		if availabilityZone != "" && volume.AvailabilityZone != "" && volume.AvailabilityZone != availabilityZone {
			return "", fmt.Errorf("volume %s is in zone %s: %w", volumeID, volume.AvailabilityZone, ErrNotFound)
		}
		for _, attachment := range volume.Attachments {
			if attachment.InstanceID == instanceID {
				return guestDevicePath(volumeID), nil
			}
		}
		timer.Reset(c.poll)
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("volume %s is not attached to %s: %w", volumeID, instanceID, ErrUnavailable)
		case <-timer.C:
		}
	}
}

func (c *Client) DetachVolume(ctx context.Context, volumeID, instanceID string, availabilityZone string) error {
	volume, err := c.GetVolumeByID(ctx, volumeID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if availabilityZone != "" && volume.AvailabilityZone != "" && volume.AvailabilityZone != availabilityZone {
		return nil
	}
	attached := false
	for _, attachment := range volume.Attachments {
		if attachment.InstanceID == instanceID {
			attached = true
			break
		}
	}
	if !attached {
		return nil
	}
	_, err = c.write(ctx, http.MethodPost, "/volumes/"+url.PathEscape(volumeID)+"/detach", nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) CreateSnapshot(ctx context.Context, spec SnapshotSpec) (*Snapshot, error) {
	accepted, err := c.write(ctx, http.MethodPost, "/volumes/"+url.PathEscape(spec.VolumeID)+"/snapshots", createSnapshotBody{
		Name: spec.Name,
	})
	if err != nil {
		return nil, err
	}
	if accepted.ResourceID == "" {
		return nil, fmt.Errorf("pantech response missing resource_id: %w", ErrInvalid)
	}
	snapshot, err := c.getSnapshot(ctx, accepted.ResourceID, spec.AvailabilityZone)
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (c *Client) DeleteSnapshot(ctx context.Context, id string, _ string) error {
	_, err := c.write(ctx, http.MethodDelete, "/snapshots/"+url.PathEscape(id), nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) ListSnapshots(ctx context.Context, page Page) ([]Snapshot, string, error) {
	if page.Name != "" || page.AvailabilityZone != "" {
		snapshots, err := c.collectSnapshots(ctx, page)
		if err != nil {
			return nil, "", err
		}
		return paginate(snapshots, page)
	}
	query := url.Values{}
	query.Set("desired_state", "present")
	if page.Size > 0 {
		query.Set("limit", strconv.Itoa(pageLimit(page.Size)))
	}
	if page.Token != "" {
		query.Set("cursor", page.Token)
	}
	var response pageResponse[apiSnapshot]
	if err := c.do(ctx, http.MethodGet, "/snapshots", query, nil, "", decodeJSON(&response)); err != nil {
		return nil, "", err
	}
	snapshots := make([]Snapshot, 0, len(response.Data))
	for _, item := range response.Data {
		snapshot, err := item.toDomain(c.config.AvailabilityZone)
		if err != nil {
			return nil, "", err
		}
		if snapshot.VolumeID == "" {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, deref(response.NextCursor), nil
}

func (c *Client) collectSnapshots(ctx context.Context, page Page) ([]Snapshot, error) {
	query := url.Values{}
	query.Set("limit", "100")
	query.Set("desired_state", "present")
	if page.Name != "" {
		query.Set("q", page.Name)
	}
	items, err := collectPages[apiSnapshot](ctx, c, "/snapshots", query)
	if err != nil {
		return nil, err
	}
	fallback := cmp.Or(page.AvailabilityZone, c.config.AvailabilityZone)
	snapshots := make([]Snapshot, 0, len(items))
	for _, item := range items {
		snapshot, err := item.toDomain(fallback)
		if err != nil {
			return nil, err
		}
		if snapshot.VolumeID == "" {
			continue
		}
		if page.Name != "" && snapshot.Name != page.Name {
			continue
		}
		if page.AvailabilityZone != "" && snapshot.AvailabilityZone != page.AvailabilityZone {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	slices.SortFunc(snapshots, func(left Snapshot, right Snapshot) int {
		return cmp.Compare(left.ID, right.ID)
	})
	return snapshots, nil
}

func (c *Client) getSnapshot(ctx context.Context, id string, availabilityZone string) (Snapshot, error) {
	var payload apiSnapshot
	if err := c.do(ctx, http.MethodGet, "/snapshots/"+url.PathEscape(id), nil, nil, "", decodeJSON(&payload)); err != nil {
		return Snapshot{}, err
	}
	return payload.toDomain(cmp.Or(availabilityZone, c.config.AvailabilityZone))
}

func (c *Client) GetInstanceMetadata(ctx context.Context) (*InstanceMetadata, error) {
	instance, err := metadata.ReadCloudInit(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("read Pantech instance metadata: %w: %w", err, ErrUnavailable)
	}
	return &InstanceMetadata{
		InstanceID:       instance.ID,
		AvailabilityZone: instance.AvailabilityZone,
		Region:           instance.Region,
	}, nil
}

func (c *Client) write(ctx context.Context, method string, path string, body any) (acceptedResponse, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = marshalJSON(body)
		if err != nil {
			return acceptedResponse{}, err
		}
	}
	key, err := newIdempotencyKey()
	if err != nil {
		return acceptedResponse{}, err
	}
	var accepted acceptedResponse
	if err := c.do(ctx, method, path, nil, payload, key, decodeJSON(&accepted)); err != nil {
		return acceptedResponse{}, err
	}
	if accepted.OperationID == "" {
		return acceptedResponse{}, fmt.Errorf("pantech response missing operation_id: %w", ErrInvalid)
	}
	if _, err := c.waitOperation(ctx, accepted.OperationID); err != nil {
		return acceptedResponse{}, err
	}
	return accepted, nil
}

func (c *Client) waitOperation(ctx context.Context, operationID string) (apiOperation, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		var operation apiOperation
		if err := c.do(ctx, http.MethodGet, "/operations/"+url.PathEscape(operationID), nil, nil, "", decodeJSON(&operation)); err != nil {
			return apiOperation{}, err
		}
		switch operation.Status {
		case "succeeded":
			return operation, nil
		case "failed":
			return apiOperation{}, operationFailure(operation)
		}
		timer.Reset(c.poll)
		select {
		case <-ctx.Done():
			return apiOperation{}, fmt.Errorf("wait for operation %s: %w", operationID, ErrUnavailable)
		case <-timer.C:
		}
	}
}

func collectPages[T any](ctx context.Context, client *Client, path string, query url.Values) ([]T, error) {
	seen := map[string]struct{}{}
	collected := make([]T, 0)
	for range maxListPages {
		var response pageResponse[T]
		if err := client.do(ctx, http.MethodGet, path, query, nil, "", decodeJSON(&response)); err != nil {
			return nil, err
		}
		collected = append(collected, response.Data...)
		cursor := deref(response.NextCursor)
		if cursor == "" {
			return collected, nil
		}
		if _, ok := seen[cursor]; ok {
			return nil, fmt.Errorf("repeated list cursor: %w", ErrInvalid)
		}
		seen[cursor] = struct{}{}
		query.Set("cursor", cursor)
	}
	return nil, fmt.Errorf("list exceeded %d pages: %w", maxListPages, ErrUnavailable)
}

func (c *Client) do(ctx context.Context, method string, path string, query url.Values, body []byte, idempotencyKey string, decode func(io.Reader) error) error {
	var last error
	for attempt := range maxAttempts {
		err := c.doOnce(ctx, method, path, query, body, idempotencyKey, decode)
		if err == nil {
			return nil
		}
		var retry *retrySignal
		if !errors.As(err, &retry) {
			return err
		}
		last = retry.err
		if attempt == maxAttempts-1 {
			break
		}
		if err := sleep(ctx, retry.after); err != nil {
			return err
		}
	}
	return last
}

func (c *Client) doOnce(ctx context.Context, method string, path string, query url.Values, body []byte, idempotencyKey string, decode func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, query), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Pantech request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return &retrySignal{
			after: c.retryDelay(nil),
			err:   fmt.Errorf("call Pantech API: %w", errors.Join(ErrUnavailable, err)),
		}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		mapped := mapHTTPError(resp)
		return &retrySignal{after: c.retryDelay(resp), err: mapped}
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return mapHTTPError(resp)
	}
	if decode == nil {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return fmt.Errorf("drain Pantech response: %w", err)
		}
		return nil
	}
	return decode(resp.Body)
}

func (c *Client) endpoint(path string, query url.Values) string {
	endpoint := *c.base
	endpoint.Path = strings.TrimRight(c.base.Path, "/") + "/" + strings.TrimLeft(path, "/")
	endpoint.RawQuery = ""
	if len(query) > 0 {
		endpoint.RawQuery = query.Encode()
	}
	return endpoint.String()
}

func (c *Client) retryDelay(resp *http.Response) time.Duration {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if after := retryAfter(resp); after > 0 {
			return after
		}
	}
	if c.poll > 0 {
		return c.poll
	}
	return time.Second
}

func retryAfter(resp *http.Response) time.Duration {
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type retrySignal struct {
	after time.Duration
	err   error
}

func (e *retrySignal) Error() string { return e.err.Error() }

func (e *retrySignal) Unwrap() error { return e.err }

func marshalJSON(body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode Pantech request: %w", err)
	}
	return data, nil
}

func decodeJSON(target any) func(io.Reader) error {
	return func(source io.Reader) error {
		if err := json.NewDecoder(source).Decode(target); err != nil {
			return fmt.Errorf("decode Pantech response: %w", errors.Join(ErrInvalid, err))
		}
		return nil
	}
}

func mapHTTPError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read Pantech error response: %w", errors.Join(ErrUnavailable, err))
	}
	var problem apiProblem
	if len(body) > 0 {
		_ = json.Unmarshal(body, &problem)
	}
	message := cmp.Or(problem.Detail, problem.Title, problem.Code, http.StatusText(resp.StatusCode))
	if len(problem.Errors) > 0 {
		parts := make([]string, 0, len(problem.Errors))
		for _, item := range problem.Errors {
			parts = append(parts, item.Field+": "+item.Message)
		}
		message += " (" + strings.Join(parts, "; ") + ")"
	}
	return mapCode(resp.StatusCode, problem.Code, message)
}

func mapCode(status int, code string, message string) error {
	switch code {
	case "RESOURCE_NOT_FOUND":
		return fmt.Errorf("%s: %w", message, ErrNotFound)
	case "UNAUTHENTICATED", "INSUFFICIENT_SCOPE", "FORBIDDEN":
		return fmt.Errorf("%s: %w", message, ErrUnauthorized)
	case "RATE_LIMITED":
		return fmt.Errorf("%s: %w", message, ErrRateLimited)
	case "INVALID_RESOURCE_STATE", "IDEMPOTENCY_CONFLICT", "CONCURRENT_UPDATE", "ACCOUNT_SUSPENDED":
		return fmt.Errorf("%s: %w", message, ErrConflict)
	case "INSUFFICIENT_CREDIT":
		return fmt.Errorf("%s: %w", message, ErrQuotaExceeded)
	case "VALIDATION_FAILED", "INVALID_REQUEST_BODY", "IDEMPOTENCY_KEY_REQUIRED", "INVALID_IDEMPOTENCY_KEY", "INVALID_FILTER", "INVALID_CURSOR":
		return fmt.Errorf("%s: %w", message, ErrInvalid)
	case "PROVISIONING_UNAVAILABLE":
		return fmt.Errorf("%s: %w", message, ErrUnavailable)
	}
	if strings.Contains(code, "QUOTA") {
		return fmt.Errorf("%s: %w", message, ErrQuotaExceeded)
	}
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%s: %w", message, ErrNotFound)
	case http.StatusConflict:
		return fmt.Errorf("%s: %w", message, ErrConflict)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s: %w", message, ErrUnauthorized)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", message, ErrRateLimited)
	default:
		if status >= http.StatusInternalServerError || status == 0 {
			return fmt.Errorf("%s: %w", message, ErrUnavailable)
		}
		return fmt.Errorf("%s: %w", message, ErrInvalid)
	}
}

func operationFailure(operation apiOperation) error {
	if operation.Failure == nil {
		return fmt.Errorf("operation %s failed: %w", operation.ID, ErrUnavailable)
	}
	return mapCode(0, operation.Failure.Code, cmp.Or(operation.Failure.Reason, operation.Failure.Code))
}

func newIdempotencyKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create idempotency key: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
}

func pageLimit(size int) int {
	if size > maxPageSize {
		return maxPageSize
	}
	return size
}

func bytesToGiB(sizeBytes int64) int {
	if sizeBytes <= 0 {
		return 0
	}
	return int((sizeBytes + bytesPerGiB - 1) / bytesPerGiB)
}

func guestDevicePath(volumeID string) string {
	return "/dev/disk/by-id/virtio-" + volumeID
}

func paginate[T any](items []T, page Page) ([]T, string, error) {
	start := 0
	if page.Token != "" {
		parsed, err := strconv.Atoi(page.Token)
		if err != nil || parsed < 0 {
			return nil, "", fmt.Errorf("invalid page token: %w", ErrInvalid)
		}
		start = parsed
	}
	if start >= len(items) {
		return nil, "", nil
	}
	if page.Size <= 0 || start+page.Size >= len(items) {
		return items[start:], "", nil
	}
	next := start + page.Size
	return items[start:next], strconv.Itoa(next), nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type createVolumeBody struct {
	Name             string `json:"name"`
	DiskOfferingSlug string `json:"disk_offering_slug"`
	SizeGB           int    `json:"size_gb,omitempty"`
	Region           string `json:"region,omitempty"`
}

type restoreSnapshotBody struct {
	Name             string `json:"name"`
	DiskOfferingSlug string `json:"disk_offering_slug"`
	SizeGB           int    `json:"size_gb,omitempty"`
}

type attachVolumeBody struct {
	InstanceID string `json:"instance_id"`
}

type resizeVolumeBody struct {
	DiskOfferingSlug string `json:"disk_offering_slug"`
	SizeGB           int    `json:"size_gb,omitempty"`
}

type createSnapshotBody struct {
	Name string `json:"name"`
}

type acceptedResponse struct {
	OperationID string `json:"operation_id"`
	ResourceID  string `json:"resource_id"`
	Status      string `json:"status"`
}

type pageResponse[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

type apiProblem struct {
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Errors []struct {
		Field   string `json:"field"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

type apiFailure struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

type apiOperation struct {
	ID         string      `json:"id"`
	ResourceID string      `json:"resource_id"`
	Status     string      `json:"status"`
	Failure    *apiFailure `json:"failure"`
}

type apiVolume struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	SizeGB           int     `json:"size_gb"`
	DiskOfferingSlug string  `json:"disk_offering_slug"`
	Region           *string `json:"region"`
	Zone             *string `json:"zone"`
	AttachedInstance *string `json:"attached_instance_id"`
	DesiredState     string  `json:"desired_state"`
	ObservedState    string  `json:"observed_state"`
}

func (v apiVolume) toDomain(fallbackZone string) Volume {
	zone := deref(v.Zone)
	if zone == "" {
		zone = fallbackZone
	}
	attachments := []Attachment(nil)
	if instanceID := deref(v.AttachedInstance); instanceID != "" {
		attachments = []Attachment{{
			InstanceID: instanceID,
			DevicePath: guestDevicePath(v.ID),
		}}
	}
	return Volume{
		ID:               v.ID,
		Name:             v.Name,
		SizeBytes:        int64(v.SizeGB) * bytesPerGiB,
		Status:           volumeStatus(v.ObservedState, v.DesiredState, len(attachments) > 0),
		AvailabilityZone: zone,
		Type:             v.DiskOfferingSlug,
		Attachments:      attachments,
	}
}

func volumeStatus(observed string, desired string, attached bool) VolumeStatus {
	if desired == "deleted" || observed == "deleting" || observed == "deleted" {
		return VolumeStatusDeleting
	}
	switch observed {
	case "pending", "provisioning":
		return VolumeStatusCreating
	case "active":
		if attached {
			return VolumeStatusInUse
		}
		return VolumeStatusAvailable
	default:
		return VolumeStatusError
	}
}

type apiSnapshot struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	VolumeID      *string `json:"volume_id"`
	SizeBytes     int64   `json:"size_bytes"`
	DesiredState  string  `json:"desired_state"`
	ObservedState string  `json:"observed_state"`
	CreatedAt     *string `json:"created_at"`
}

func (s apiSnapshot) toDomain(fallbackZone string) (Snapshot, error) {
	createdAt, err := parseTime(s.CreatedAt)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		ID:               s.ID,
		Name:             s.Name,
		VolumeID:         deref(s.VolumeID),
		SizeBytes:        s.SizeBytes,
		Status:           snapshotStatus(s.ObservedState, s.DesiredState),
		CreatedAt:        createdAt,
		AvailabilityZone: fallbackZone,
	}, nil
}

func snapshotStatus(observed string, desired string) SnapshotStatus {
	if desired == "deleted" || observed == "deleting" || observed == "deleted" {
		return SnapshotStatusDeleting
	}
	switch observed {
	case "pending", "provisioning":
		return SnapshotStatusCreating
	case "active":
		return SnapshotStatusAvailable
	default:
		return SnapshotStatusError
	}
}

func parseTime(value *string) (time.Time, error) {
	if value == nil || *value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, *value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse Pantech timestamp %q: %w", *value, ErrInvalid)
	}
	return parsed, nil
}
