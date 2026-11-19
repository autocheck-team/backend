package material

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/autocheck-team/backend/internal/storage"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// startObjects использует отдельный SeaweedFS с динамическим портом и удаляет только свой контейнер.
func startObjects(t *testing.T) *storage.Storage {
	t.Helper()
	docker, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := docker.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if _, err := docker.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	const image = "chrislusf/seaweedfs:4.53"
	if _, err := docker.ImageInspect(ctx, image); err != nil {
		pull, err := docker.ImagePull(ctx, image, client.ImagePullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		waitErr := pull.Wait(ctx)
		if err := errors.Join(waitErr, pull.Close()); err != nil {
			t.Fatal(err)
		}
	}
	port, err := network.ParsePort("8333/tcp")
	if err != nil {
		t.Fatal(err)
	}
	created, err := docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       "codex-material-s3-" + uuid.NewString(),
		Config:     &container.Config{Image: image, Cmd: []string{"server", "-s3", "-dir=/data", "-ip=127.0.0.1", "-ip.bind=0.0.0.0", "-master.volumeSizeLimitMB=32", "-volume.max=8", "-master.telemetry=false"}, ExposedPorts: network.PortSet{port: {}}},
		HostConfig: &container.HostConfig{PortBindings: network.PortMap{port: {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "0"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := docker.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			t.Error(err)
		}
	})
	if _, err := docker.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := docker.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bindings := info.Container.NetworkSettings.Ports[port]
	if len(bindings) != 1 {
		t.Fatal("S3 port not bound")
	}
	endpoint := net.JoinHostPort("127.0.0.1", bindings[0].HostPort)
	for {
		objects, err := storage.New(ctx, storage.Config{Endpoint: endpoint, Bucket: "material-test", AccessKey: "material-test", SecretKey: "material-test"})
		if err == nil {
			return objects
		}
		select {
		case <-ctx.Done():
			t.Fatalf("S3 startup: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func transfer(t *testing.T, method, address string, data []byte) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, address, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := httpClient.Do(request)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) {
			t.Fatalf("S3 transfer: %v", urlError.Err)
		}
		t.Fatal("S3 transfer failed")
	}
	result, readErr := io.ReadAll(response.Body)
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("S3 transfer status=%d", response.StatusCode)
	}
	return result
}

func TestGatewayMaterialS3(t *testing.T) {
	// arrange
	f := setup(t)
	objects := startObjects(t)
	f.svc.objects = objects
	material := f.create(t, true, 0)
	g := startGateway(t, f)
	path := "/v1/materials/" + material.ID.String()
	upload := &v1.CreateMaterialAttachmentResponse{}
	g.call(t, f.admin, http.MethodPost, path+"/attachments", &v1.CreateMaterialAttachmentRequest{Filename: "lesson.pdf", RequestId: uuid.NewString()}, upload, http.StatusOK)
	data := []byte("%PDF-1.7\nlesson")
	// act
	transfer(t, http.MethodPut, upload.GetUploadUrl(), data)
	confirmPath := path + "/attachments/" + upload.GetAttachment().GetId() + ":confirm"
	g.call(t, f.admin, http.MethodPost, confirmPath, &v1.ConfirmMaterialAttachmentRequest{}, &v1.ConfirmMaterialAttachmentResponse{}, http.StatusOK)
	transfer(t, http.MethodPut, upload.GetUploadUrl(), []byte("%PDF-1.7\nmodified"))
	get := &v1.GetMaterialResponse{}
	g.call(t, f.admin, http.MethodGet, path, nil, get, http.StatusOK)
	// assert
	if len(get.GetMaterial().GetAttachments()) != 1 {
		t.Fatal("missing confirmed attachment")
	}
	downloadURL := get.GetMaterial().GetAttachments()[0].GetDownloadUrl()
	if !bytes.Equal(transfer(t, http.MethodGet, downloadURL, nil), data) {
		t.Fatal("confirmed file changed through old PUT link")
	}
	g.call(t, f.student, http.MethodGet, path, nil, get, http.StatusOK)
	if !get.GetMaterial().GetIsLocked() || len(get.GetMaterial().GetAttachments()) != 0 {
		t.Fatal("free user received attachments")
	}
	g.call(t, f.admin, http.MethodDelete, path, nil, &v1.DeleteMaterialResponse{}, http.StatusOK)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := objects.Get(ctx, attachmentPrefix(material.ID, uuid.MustParse(upload.GetAttachment().GetId()))+"upload"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("S3 cleanup: %v", err)
	}
	t.Log("REST + PostgreSQL + SeaweedFS: presigned PUT, confirm, immutable GET and cleanup passed")
}
