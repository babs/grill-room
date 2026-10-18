package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type k8sMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	ReadOnly  bool   `yaml:"readOnly"`
}

type k8sContainer struct {
	Name            string     `yaml:"name"`
	Image           string     `yaml:"image"`
	Args            []string   `yaml:"args"`
	VolumeMounts    []k8sMount `yaml:"volumeMounts"`
	SecurityContext struct {
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		ReadOnlyRootFilesystem   bool  `yaml:"readOnlyRootFilesystem"`
		Capabilities             struct {
			Drop []string `yaml:"drop"`
		} `yaml:"capabilities"`
	} `yaml:"securityContext"`
	Env []k8sEnv `yaml:"env"`
}

type k8sEnv struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type k8sVolume struct {
	Name     string `yaml:"name"`
	EmptyDir *struct {
		SizeLimit string `yaml:"sizeLimit"`
	} `yaml:"emptyDir"`
	PersistentVolumeClaim struct {
		ClaimName string `yaml:"claimName"`
	} `yaml:"persistentVolumeClaim"`
}

type k8sDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				SecurityContext struct {
					RunAsNonRoot bool `yaml:"runAsNonRoot"`
				} `yaml:"securityContext"`
				InitContainers []k8sContainer `yaml:"initContainers"`
				Containers     []k8sContainer `yaml:"containers"`
				Volumes        []k8sVolume    `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func mountOf(c k8sContainer, volume string) (k8sMount, bool) {
	for _, m := range c.VolumeMounts {
		if m.Name == volume {
			return m, true
		}
	}
	return k8sMount{}, false
}

func roomManifest(t *testing.T) []k8sDoc {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/k8s/room.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var docs []k8sDoc
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var d k8sDoc
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, d)
	}
	return docs
}

// The staging directory must be one the server creates and owns: an emptyDir's mount point
// belongs to root, and PrepareStaging refuses it.
func TestRoomManifestStagingDir(t *testing.T) {
	docs := roomManifest(t)
	di := slices.IndexFunc(docs, func(d k8sDoc) bool { return d.Kind == "Deployment" && d.Metadata.Name == "grill-room" })
	if di < 0 {
		t.Fatal("no grill-room Deployment")
	}
	pod := docs[di].Spec.Template.Spec
	server := pod.Containers[0]
	ei := slices.IndexFunc(server.Env, func(e k8sEnv) bool { return e.Name == "GRILL_ROOM_STAGING_DIR" })
	if ei < 0 {
		t.Fatal("GRILL_ROOM_STAGING_DIR unset")
	}
	dir := path.Clean(server.Env[ei].Value)
	below := false
	for _, m := range server.VolumeMounts {
		mp := path.Clean(m.MountPath)
		if dir == mp {
			t.Errorf("GRILL_ROOM_STAGING_DIR %q is the mount point of volume %q", dir, m.Name)
		}
		vi := slices.IndexFunc(pod.Volumes, func(v k8sVolume) bool { return v.Name == m.Name })
		if vi >= 0 && pod.Volumes[vi].EmptyDir != nil && !m.ReadOnly && strings.HasPrefix(dir, mp+"/") {
			below = true
		}
	}
	if !below {
		t.Errorf("GRILL_ROOM_STAGING_DIR %q is not below a writable emptyDir mount", dir)
	}
}

// 010-AC7: the server image fills a dedicated PVC from a non-root, read-only init container, and
// the server mounts it read-only where GRILL_ROOM_DEVICE_STT_MODEL_DIR points.
func TestRoomManifestModelsInit(t *testing.T) {
	docs := roomManifest(t)
	pvc := slices.IndexFunc(docs, func(d k8sDoc) bool {
		return d.Kind == "PersistentVolumeClaim" && d.Metadata.Name == "grill-room-models"
	})
	if pvc < 0 {
		t.Fatal("no grill-room-models PVC")
	}
	di := slices.IndexFunc(docs, func(d k8sDoc) bool { return d.Kind == "Deployment" && d.Metadata.Name == "grill-room" })
	if di < 0 {
		t.Fatal("no grill-room Deployment")
	}
	pod := docs[di].Spec.Template.Spec
	if !pod.SecurityContext.RunAsNonRoot {
		t.Error("pod is not runAsNonRoot")
	}
	vi := slices.IndexFunc(pod.Volumes, func(v k8sVolume) bool {
		return v.PersistentVolumeClaim.ClaimName == "grill-room-models"
	})
	if vi < 0 {
		t.Fatal("no volume on the grill-room-models claim")
	}
	volume := pod.Volumes[vi].Name
	if len(pod.InitContainers) != 1 || len(pod.Containers) != 1 {
		t.Fatalf("want one init container and one container, got %d and %d", len(pod.InitContainers), len(pod.Containers))
	}
	init, server := pod.InitContainers[0], pod.Containers[0]
	if init.Image != server.Image {
		t.Errorf("init image %q, server image %q: the fetch must run from the server image", init.Image, server.Image)
	}
	im, ok := mountOf(init, volume)
	if !ok || im.ReadOnly {
		t.Fatalf("init container mount %+v: want the models volume, writable", im)
	}
	wantArgs := []string{"models", "fetch", "--best-effort", "--dir", im.MountPath}
	if !slices.Equal(init.Args, wantArgs) {
		t.Errorf("init args %q, want %q", init.Args, wantArgs)
	}
	sc := init.SecurityContext
	if !sc.ReadOnlyRootFilesystem || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || !slices.Equal(sc.Capabilities.Drop, []string{"ALL"}) {
		t.Errorf("init container securityContext %+v: want a read-only root, no escalation, all capabilities dropped", sc)
	}
	sm, ok := mountOf(server, volume)
	if !ok || !sm.ReadOnly {
		t.Fatalf("server mount %+v: want the models volume, read-only", sm)
	}
	ei := slices.IndexFunc(server.Env, func(e k8sEnv) bool {
		return e.Name == "GRILL_ROOM_DEVICE_STT_MODEL_DIR"
	})
	if ei < 0 || server.Env[ei].Value != sm.MountPath {
		t.Errorf("GRILL_ROOM_DEVICE_STT_MODEL_DIR must name the read-only mount %q", sm.MountPath)
	}
}
