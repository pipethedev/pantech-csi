package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestClient_CreateVolume_PollsOperation(t *testing.T) {
	keys := []string{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/public/v1/volumes":
			if r.Method != http.MethodPost {
				t.Fatalf("expected POST, got %s", r.Method)
			}
			if r.Header.Get("Authorization") != "Bearer api-key" {
				t.Fatalf("expected bearer auth, got %q", r.Header.Get("Authorization"))
			}
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			var body createVolumeBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if body.Name != "data" || body.DiskOfferingSlug != "ssd" || body.SizeGB != 2 || body.Region != "af-abj" {
				t.Fatalf("unexpected request body: %+v", body)
			}
			return jsonResponse(t, http.StatusAccepted, acceptedResponse{
				OperationID: "op_1",
				ResourceID:  "vol_1",
				Status:      "submitting",
			}), nil
		case "/public/v1/operations/op_1":
			return jsonResponse(t, http.StatusOK, apiOperation{ID: "op_1", Status: "succeeded", ResourceID: "vol_1"}), nil
		case "/public/v1/volumes/vol_1":
			return jsonResponse(t, http.StatusOK, apiVolume{
				ID:               "vol_1",
				Name:             "data",
				SizeGB:           2,
				DiskOfferingSlug: "ssd",
				Zone:             strPtr("zone-a"),
				DesiredState:     "present",
				ObservedState:    "active",
			}), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	client := newTestClient(t, transport)
	volume, err := client.CreateVolume(context.Background(), VolumeSpec{
		Name:             "data",
		SizeBytes:        2 * bytesPerGiB,
		Type:             "ssd",
		Region:           "af-abj",
		AvailabilityZone: "zone-a",
	})
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	if volume.ID != "vol_1" || volume.SizeBytes != 2*bytesPerGiB || volume.AvailabilityZone != "zone-a" || volume.Type != "ssd" {
		t.Fatalf("unexpected volume: %+v", volume)
	}
	if volume.Status != VolumeStatusAvailable {
		t.Fatalf("expected available volume, got %s", volume.Status)
	}
	if len(keys) != 1 || keys[0] == "" {
		t.Fatalf("expected one idempotency key, got %v", keys)
	}
}

func TestClient_CreateVolume_FromSnapshotUsesRestore(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/public/v1/snapshots/snap_1/restore":
			var body restoreSnapshotBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if body.Name != "data" || body.DiskOfferingSlug != "ssd" || body.SizeGB != 2 {
				t.Fatalf("unexpected restore body: %+v", body)
			}
			return jsonResponse(t, http.StatusAccepted, acceptedResponse{OperationID: "op_1", ResourceID: "vol_1"}), nil
		case "/public/v1/operations/op_1":
			return jsonResponse(t, http.StatusOK, apiOperation{ID: "op_1", Status: "succeeded"}), nil
		case "/public/v1/volumes/vol_1":
			return jsonResponse(t, http.StatusOK, apiVolume{
				ID:               "vol_1",
				Name:             "data",
				SizeGB:           2,
				DiskOfferingSlug: "ssd",
				Zone:             strPtr("zone-a"),
				DesiredState:     "present",
				ObservedState:    "active",
			}), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	client := newTestClient(t, transport)
	volume, err := client.CreateVolume(context.Background(), VolumeSpec{
		Name:       "data",
		SizeBytes:  2 * bytesPerGiB,
		Type:       "ssd",
		SnapshotID: "snap_1",
	})
	if err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	if volume.ID != "vol_1" {
		t.Fatalf("unexpected volume: %+v", volume)
	}
}

func TestClient_CreateVolume_MissingDiskOffering(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("expected no request")
		return nil, nil
	}))
	_, err := client.CreateVolume(context.Background(), VolumeSpec{Name: "data", SizeBytes: bytesPerGiB})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestClient_CreateVolume_RetriesWithSameIdempotencyKey(t *testing.T) {
	posts := 0
	keys := []string{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/public/v1/volumes":
			posts++
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			if posts == 1 {
				return problemResponse(t, http.StatusServiceUnavailable, "PROVISIONING_UNAVAILABLE", "try again"), nil
			}
			return jsonResponse(t, http.StatusAccepted, acceptedResponse{OperationID: "op_1", ResourceID: "vol_1"}), nil
		case "/public/v1/operations/op_1":
			return jsonResponse(t, http.StatusOK, apiOperation{ID: "op_1", Status: "succeeded"}), nil
		case "/public/v1/volumes/vol_1":
			return jsonResponse(t, http.StatusOK, apiVolume{
				ID:               "vol_1",
				SizeGB:           1,
				DiskOfferingSlug: "ssd",
				DesiredState:     "present",
				ObservedState:    "active",
			}), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	client := newTestClient(t, transport)
	if _, err := client.CreateVolume(context.Background(), VolumeSpec{
		Name:      "data",
		SizeBytes: bytesPerGiB,
		Type:      "ssd",
	}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("expected the same idempotency key on retry, got %v", keys)
	}
}

func TestClient_GetVolumeByID_UsesVolumePath(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/public/v1/volumes/vol_1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		return jsonResponse(t, http.StatusOK, apiVolume{
			ID:               "vol_1",
			Name:             "data",
			SizeGB:           1,
			DiskOfferingSlug: "ssd",
			Zone:             strPtr("zone-a"),
			AttachedInstance: strPtr("inst_1"),
			DesiredState:     "present",
			ObservedState:    "active",
		}), nil
	})
	client := newTestClient(t, transport)
	volume, err := client.GetVolumeByID(context.Background(), "vol_1")
	if err != nil {
		t.Fatalf("get volume: %v", err)
	}
	if volume.Status != VolumeStatusInUse || len(volume.Attachments) != 1 {
		t.Fatalf("unexpected volume: %+v", volume)
	}
	if volume.Attachments[0].InstanceID != "inst_1" || volume.Attachments[0].DevicePath != guestDevicePath("vol_1") {
		t.Fatalf("unexpected attachment: %+v", volume.Attachments)
	}
}

func TestClient_GetVolumeByID_NotFound(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return problemResponse(t, http.StatusNotFound, "RESOURCE_NOT_FOUND", "missing"), nil
	})
	client := newTestClient(t, transport)
	_, err := client.GetVolumeByID(context.Background(), "vol_1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestClient_GetVolumeByID_InvalidBody(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(`[]`))),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})
	client := newTestClient(t, transport)
	_, err := client.GetVolumeByID(context.Background(), "vol_1")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestNewClient_MissingAPIKeyReturnsError(t *testing.T) {
	_, err := NewClient(ClientConfig{BaseURL: "https://api.pantechdynamics.com"})
	if err == nil {
		t.Fatal("expected missing API key error")
	}
}

func TestClient_UnauthorizedDoesNotRetry(t *testing.T) {
	requests := 0
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return problemResponse(t, http.StatusUnauthorized, "UNAUTHENTICATED", "bad key"), nil
	})
	client := newTestClient(t, transport)
	_, err := client.GetVolumeByID(context.Background(), "vol_1")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("expected one request, got %d", requests)
	}
}

func TestClient_DeleteVolume_NotFoundIsSuccess(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodDelete || r.URL.Path != "/public/v1/volumes/vol_1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Fatal("expected idempotency key")
		}
		return problemResponse(t, http.StatusNotFound, "RESOURCE_NOT_FOUND", "missing"), nil
	})
	client := newTestClient(t, transport)
	if err := client.DeleteVolume(context.Background(), "vol_1"); err != nil {
		t.Fatalf("delete volume: %v", err)
	}
}

func TestClient_AttachVolume_WaitsForInstance(t *testing.T) {
	gets := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/public/v1/volumes/vol_1/attach":
			var body attachVolumeBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode attach request: %v", err)
			}
			if body.InstanceID != "inst_1" {
				t.Fatalf("expected inst_1, got %q", body.InstanceID)
			}
			return jsonResponse(t, http.StatusAccepted, acceptedResponse{OperationID: "op_1", ResourceID: "vol_1"}), nil
		case "/public/v1/operations/op_1":
			return jsonResponse(t, http.StatusOK, apiOperation{ID: "op_1", Status: "succeeded"}), nil
		case "/public/v1/volumes/vol_1":
			gets++
			volume := apiVolume{
				ID:            "vol_1",
				SizeGB:        1,
				Zone:          strPtr("zone-a"),
				DesiredState:  "present",
				ObservedState: "active",
			}
			if gets > 1 {
				volume.AttachedInstance = strPtr("inst_1")
			}
			return jsonResponse(t, http.StatusOK, volume), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	client := newTestClient(t, transport)
	device, err := client.AttachVolume(context.Background(), "vol_1", "inst_1", "zone-a")
	if err != nil {
		t.Fatalf("attach volume: %v", err)
	}
	if device != guestDevicePath("vol_1") {
		t.Fatalf("expected %s, got %q", guestDevicePath("vol_1"), device)
	}
}

func TestClient_AttachVolume_FailedOperation(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/public/v1/volumes/vol_1/attach":
			return jsonResponse(t, http.StatusAccepted, acceptedResponse{OperationID: "op_1", ResourceID: "vol_1"}), nil
		case "/public/v1/operations/op_1":
			return jsonResponse(t, http.StatusOK, apiOperation{
				ID:     "op_1",
				Status: "failed",
				Failure: &apiFailure{
					Code:   "INVALID_RESOURCE_STATE",
					Reason: "instance is stopped",
				},
			}), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	client := newTestClient(t, transport)
	_, err := client.AttachVolume(context.Background(), "vol_1", "inst_1", "zone-a")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestClient_DetachVolume_UsesDetachPath(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/public/v1/volumes/vol_1":
			return jsonResponse(t, http.StatusOK, apiVolume{
				ID:               "vol_1",
				Zone:             strPtr("zone-a"),
				AttachedInstance: strPtr("inst_1"),
				DesiredState:     "present",
				ObservedState:    "active",
			}), nil
		case "/public/v1/volumes/vol_1/detach":
			if r.Method != http.MethodPost {
				t.Fatalf("expected POST, got %s", r.Method)
			}
			if r.Header.Get("Idempotency-Key") == "" {
				t.Fatal("expected idempotency key")
			}
			return jsonResponse(t, http.StatusAccepted, acceptedResponse{OperationID: "op_1", ResourceID: "vol_1"}), nil
		case "/public/v1/operations/op_1":
			return jsonResponse(t, http.StatusOK, apiOperation{ID: "op_1", Status: "succeeded"}), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	client := newTestClient(t, transport)
	if err := client.DetachVolume(context.Background(), "vol_1", "inst_1", "zone-a"); err != nil {
		t.Fatalf("detach volume: %v", err)
	}
}

func TestClient_ResizeVolume_UsesCurrentOffering(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/public/v1/volumes/vol_1":
			return jsonResponse(t, http.StatusOK, apiVolume{
				ID:               "vol_1",
				SizeGB:           3,
				DiskOfferingSlug: "ssd",
				Zone:             strPtr("zone-a"),
				DesiredState:     "present",
				ObservedState:    "active",
			}), nil
		case r.Method == http.MethodPost && r.URL.Path == "/public/v1/volumes/vol_1/resize":
			var body resizeVolumeBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode resize request: %v", err)
			}
			if body.DiskOfferingSlug != "ssd" || body.SizeGB != 2 {
				t.Fatalf("unexpected resize body: %+v", body)
			}
			return jsonResponse(t, http.StatusAccepted, acceptedResponse{OperationID: "op_1", ResourceID: "vol_1"}), nil
		case r.URL.Path == "/public/v1/operations/op_1":
			return jsonResponse(t, http.StatusOK, apiOperation{ID: "op_1", Status: "succeeded"}), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	client := newTestClient(t, transport)
	size, err := client.ResizeVolume(context.Background(), "vol_1", 2*bytesPerGiB, "zone-a")
	if err != nil {
		t.Fatalf("resize volume: %v", err)
	}
	if size != 3*bytesPerGiB {
		t.Fatalf("expected provider size %d, got %d", 3*bytesPerGiB, size)
	}
}

func TestClient_CreateSnapshot_ParsesCreatedAt(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/public/v1/volumes/vol_1/snapshots":
			var body createSnapshotBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode snapshot request: %v", err)
			}
			if body.Name != "backup" {
				t.Fatalf("unexpected snapshot body: %+v", body)
			}
			return jsonResponse(t, http.StatusAccepted, acceptedResponse{OperationID: "op_1", ResourceID: "snap_1"}), nil
		case "/public/v1/operations/op_1":
			return jsonResponse(t, http.StatusOK, apiOperation{ID: "op_1", Status: "succeeded"}), nil
		case "/public/v1/snapshots/snap_1":
			return jsonResponse(t, http.StatusOK, apiSnapshot{
				ID:            "snap_1",
				Name:          "backup",
				VolumeID:      strPtr("vol_1"),
				SizeBytes:     2 * bytesPerGiB,
				DesiredState:  "present",
				ObservedState: "active",
				CreatedAt:     strPtr("2026-07-27T00:06:57Z"),
			}), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	client := newTestClient(t, transport)
	snapshot, err := client.CreateSnapshot(context.Background(), SnapshotSpec{
		Name:             "backup",
		VolumeID:         "vol_1",
		AvailabilityZone: "zone-a",
	})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if snapshot.CreatedAt.IsZero() || snapshot.SizeBytes != 2*bytesPerGiB || snapshot.Status != SnapshotStatusAvailable {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.AvailabilityZone != "zone-a" {
		t.Fatalf("expected zone-a, got %q", snapshot.AvailabilityZone)
	}
}

func TestClient_CreateSnapshot_RejectsMalformedCreatedAt(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/public/v1/volumes/vol_1/snapshots":
			return jsonResponse(t, http.StatusAccepted, acceptedResponse{OperationID: "op_1", ResourceID: "snap_1"}), nil
		case "/public/v1/operations/op_1":
			return jsonResponse(t, http.StatusOK, apiOperation{ID: "op_1", Status: "succeeded"}), nil
		case "/public/v1/snapshots/snap_1":
			return jsonResponse(t, http.StatusOK, apiSnapshot{
				ID:            "snap_1",
				Name:          "backup",
				VolumeID:      strPtr("vol_1"),
				DesiredState:  "present",
				ObservedState: "active",
				CreatedAt:     strPtr("not-a-timestamp"),
			}), nil
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			return nil, nil
		}
	})
	client := newTestClient(t, transport)
	_, err := client.CreateSnapshot(context.Background(), SnapshotSpec{Name: "backup", VolumeID: "vol_1"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestClient_ListVolumes_PassesCursor(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/public/v1/volumes" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("cursor") != "cursor-1" {
			t.Fatalf("unexpected query %s", r.URL.RawQuery)
		}
		return jsonResponse(t, http.StatusOK, pageResponse[apiVolume]{
			Data: []apiVolume{
				{ID: "vol_b", SizeGB: 1, DesiredState: "present", ObservedState: "active"},
			},
			NextCursor: strPtr("cursor-2"),
		}), nil
	})
	client := newTestClient(t, transport)
	volumes, token, err := client.ListVolumes(context.Background(), Page{Size: 2, Token: "cursor-1"})
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	if len(volumes) != 1 || volumes[0].ID != "vol_b" || token != "cursor-2" {
		t.Fatalf("unexpected page: %+v token %q", volumes, token)
	}
}

func TestClient_ListVolumes_CapsLimit(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("limit") != "100" {
			t.Fatalf("expected limit 100, got %q", r.URL.Query().Get("limit"))
		}
		return jsonResponse(t, http.StatusOK, pageResponse[apiVolume]{}), nil
	})
	client := newTestClient(t, transport)
	if _, _, err := client.ListVolumes(context.Background(), Page{Size: 500}); err != nil {
		t.Fatalf("list volumes: %v", err)
	}
}

func TestClient_ListSnapshots_SkipsInstanceSnapshots(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/public/v1/snapshots" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return jsonResponse(t, http.StatusOK, pageResponse[apiSnapshot]{
			Data: []apiSnapshot{
				{ID: "snap_root", Name: "root", DesiredState: "present", ObservedState: "active"},
				{ID: "snap_vol", Name: "data", VolumeID: strPtr("vol_1"), SizeBytes: bytesPerGiB, DesiredState: "present", ObservedState: "active"},
			},
		}), nil
	})
	client := newTestClient(t, transport)
	snapshots, _, err := client.ListSnapshots(context.Background(), Page{})
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].ID != "snap_vol" || snapshots[0].VolumeID != "vol_1" {
		t.Fatalf("unexpected snapshots: %+v", snapshots)
	}
}

func TestClient_ListVolumes_FiltersExactName(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("q") != "data" || r.URL.Query().Get("desired_state") != "present" {
			t.Fatalf("unexpected query %s", r.URL.RawQuery)
		}
		return jsonResponse(t, http.StatusOK, pageResponse[apiVolume]{
			Data: []apiVolume{
				{ID: "vol_2", Name: "data-extra", SizeGB: 1, Zone: strPtr("zone-a"), DiskOfferingSlug: "ssd", DesiredState: "present", ObservedState: "active"},
				{ID: "vol_1", Name: "data", SizeGB: 1, Zone: strPtr("zone-a"), DiskOfferingSlug: "ssd", DesiredState: "present", ObservedState: "active"},
				{ID: "vol_3", Name: "data", SizeGB: 1, Zone: strPtr("zone-b"), DiskOfferingSlug: "ssd", DesiredState: "present", ObservedState: "active"},
			},
		}), nil
	})
	client := newTestClient(t, transport)
	volumes, token, err := client.ListVolumes(context.Background(), Page{Name: "data", AvailabilityZone: "zone-a"})
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	if token != "" || len(volumes) != 1 || volumes[0].ID != "vol_1" {
		t.Fatalf("unexpected volumes: %+v token %q", volumes, token)
	}
}

func TestClient_ErrorCodes(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		want   error
	}{
		{name: "rate limit", status: http.StatusTooManyRequests, code: "RATE_LIMITED", want: ErrRateLimited},
		{name: "validation", status: http.StatusUnprocessableEntity, code: "VALIDATION_FAILED", want: ErrInvalid},
		{name: "conflict", status: http.StatusConflict, code: "IDEMPOTENCY_CONFLICT", want: ErrConflict},
		{name: "credit", status: http.StatusForbidden, code: "INSUFFICIENT_CREDIT", want: ErrQuotaExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
				resp := problemResponse(t, test.status, test.code, test.name)
				if test.status == http.StatusTooManyRequests {
					resp.Header.Set("Retry-After", "0")
				}
				return resp, nil
			})
			client := newTestClient(t, transport)
			_, err := client.GetVolumeByID(context.Background(), "vol_1")
			if !errors.Is(err, test.want) {
				t.Fatalf("expected %v, got %v", test.want, err)
			}
		})
	}
}

func TestClient_ContextCanceled_DoesNotSendRequest(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		t.Fatal("expected canceled context")
		return nil, nil
	})
	client := newTestClient(t, transport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.GetVolumeByID(ctx, "vol_1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestClient_GetInstanceMetadata_CanceledContext_ReturnsCanceled(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("expected canceled context")
		return nil, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.GetInstanceMetadata(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("canceled metadata should not be classified as unavailable: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newTestClient(t *testing.T, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{
		BaseURL:          "https://api.pantechdynamics.com",
		APIKey:           "api-key",
		Region:           "af-abj",
		AvailabilityZone: "zone-a",
		HTTPClient:       &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	client.poll = time.Millisecond
	return client
}

func jsonResponse(t *testing.T, status int, value any) *http.Response {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(data)),
		Header:     make(http.Header),
	}
}

func problemResponse(t *testing.T, status int, code string, detail string) *http.Response {
	t.Helper()
	return jsonResponse(t, status, apiProblem{
		Code:   code,
		Title:  code,
		Detail: detail,
	})
}

func strPtr(value string) *string {
	return &value
}
