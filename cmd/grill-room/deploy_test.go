package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
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
	Env       []k8sEnv `yaml:"env"`
	Lifecycle struct {
		PreStop *struct {
			Sleep *struct {
				Seconds int `yaml:"seconds"`
			} `yaml:"sleep"`
		} `yaml:"preStop"`
	} `yaml:"lifecycle"`
}

type k8sEnv struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom struct {
		SecretKeyRef struct {
			Name string `yaml:"name"`
			Key  string `yaml:"key"`
		} `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

type k8sVolume struct {
	Name     string `yaml:"name"`
	EmptyDir *struct {
		SizeLimit string `yaml:"sizeLimit"`
	} `yaml:"emptyDir"`
	PersistentVolumeClaim *struct {
		ClaimName string `yaml:"claimName"`
	} `yaml:"persistentVolumeClaim"`
}

type k8sSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// matches reports whether every label the selector names is on labels; an empty one matches nothing here.
func (s k8sSelector) matches(labels map[string]string) bool {
	if len(s.MatchLabels) == 0 {
		return false
	}
	for k, v := range s.MatchLabels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

type k8sDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Replicas *int `yaml:"replicas"`
		Strategy struct {
			Type          string `yaml:"type"`
			RollingUpdate struct {
				MaxUnavailable any `yaml:"maxUnavailable"`
			} `yaml:"rollingUpdate"`
		} `yaml:"strategy"`
		MinAvailable any         `yaml:"minAvailable"`
		Selector     k8sSelector `yaml:"selector"`
		AccessModes  []string    `yaml:"accessModes"`
		Template     struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				TerminationGracePeriodSeconds *int `yaml:"terminationGracePeriodSeconds"`
				SecurityContext               struct {
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

// placeholder is a `make k8s-render` variable; unrendered, `{ port: ${X} }` is not valid YAML.
var placeholder = regexp.MustCompile(`\$\{(\w+)\}`)

// manifest decodes every document of a file under deploy/k8s, each ${VAR} read as <VAR>.
func manifest[T any](t *testing.T, name string) []T {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/k8s/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var docs []T
	dec := yaml.NewDecoder(bytes.NewReader(placeholder.ReplaceAll(raw, []byte("<$1>"))))
	for {
		var d T
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, d)
	}
	return docs
}

func roomManifest(t *testing.T) []k8sDoc { t.Helper(); return manifest[k8sDoc](t, "room.yaml") }

func roomDeployment(t *testing.T, docs []k8sDoc) k8sDoc {
	t.Helper()
	di := slices.IndexFunc(docs, func(d k8sDoc) bool { return d.Kind == "Deployment" && d.Metadata.Name == "grill-room" })
	if di < 0 {
		t.Fatal("no grill-room Deployment")
	}
	return docs[di]
}

// The staging directory must be one the server creates and owns: an emptyDir's mount point
// belongs to root, and PrepareStaging refuses it.
func TestRoomManifestStagingDir(t *testing.T) {
	pod := roomDeployment(t, roomManifest(t)).Spec.Template.Spec
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
			// 017-AC21: past sizeLimit the kubelet evicts the pod instead of filling the node.
			if pod.Volumes[vi].EmptyDir.SizeLimit == "" {
				t.Errorf("staging volume %q has no sizeLimit", m.Name)
			}
		}
	}
	if !below {
		t.Errorf("GRILL_ROOM_STAGING_DIR %q is not below a writable emptyDir mount", dir)
	}
}

// 010-AC7: the server image fills the models volume from a non-root, read-only init container, and
// the server mounts it read-only where GRILL_ROOM_DEVICE_STT_MODEL_DIR points. 017-AC21: the
// volume is the pod's own emptyDir, so a second pod schedules on any node.
func TestRoomManifestModelsInit(t *testing.T) {
	pod := roomDeployment(t, roomManifest(t)).Spec.Template.Spec
	if !pod.SecurityContext.RunAsNonRoot {
		t.Error("pod is not runAsNonRoot")
	}
	vi := slices.IndexFunc(pod.Volumes, func(v k8sVolume) bool { return v.Name == "models" })
	if vi < 0 {
		t.Fatal("no models volume")
	}
	if ed := pod.Volumes[vi].EmptyDir; ed == nil || ed.SizeLimit == "" {
		t.Fatalf("models volume %+v: want an emptyDir with a sizeLimit", pod.Volumes[vi])
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

type k8sPeer struct {
	NamespaceSelector *k8sSelector `yaml:"namespaceSelector"`
	PodSelector       *k8sSelector `yaml:"podSelector"`
}

type k8sPort struct {
	Port     string `yaml:"port"`
	Protocol string `yaml:"protocol"`
}

type k8sNetPol struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		PodSelector k8sSelector `yaml:"podSelector"`
		PolicyTypes []string    `yaml:"policyTypes"`
		Egress      []struct {
			To    []k8sPeer `yaml:"to"`
			Ports []k8sPort `yaml:"ports"`
		} `yaml:"egress"`
	} `yaml:"spec"`
}

// 017-AC21: two pods that roll one at a time, a disruption budget that keeps one, no ReadWriteOnce
// claim that would pin them to one node, the DSN's credentials from the PG secret, and egress to PG.
func TestRoomManifestRollout(t *testing.T) {
	docs := roomManifest(t)
	dep := roomDeployment(t, docs)
	if r := dep.Spec.Replicas; r == nil || *r != 2 {
		t.Error("replicas unset or not 2")
	}
	if st := dep.Spec.Strategy; st.Type != "RollingUpdate" || fmt.Sprint(st.RollingUpdate.MaxUnavailable) != "0" {
		t.Errorf("strategy %+v: want RollingUpdate with maxUnavailable 0", st)
	}
	labels := dep.Spec.Template.Metadata.Labels
	pdbs := 0
	for _, d := range docs {
		if d.Kind == "PodDisruptionBudget" && d.Spec.Selector.matches(labels) {
			pdbs++
			if fmt.Sprint(d.Spec.MinAvailable) != "1" {
				t.Errorf("PodDisruptionBudget %s: minAvailable %v, want 1", d.Metadata.Name, d.Spec.MinAvailable)
			}
		}
	}
	if pdbs != 1 {
		t.Errorf("%d PodDisruptionBudgets select the server pods, want 1", pdbs)
	}

	pod := dep.Spec.Template.Spec
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		ci := slices.IndexFunc(docs, func(d k8sDoc) bool {
			return d.Kind == "PersistentVolumeClaim" && d.Metadata.Name == v.PersistentVolumeClaim.ClaimName
		})
		if ci < 0 || slices.Contains(docs[ci].Spec.AccessModes, "ReadWriteOnce") {
			t.Errorf("volume %q mounts claim %q: a ReadWriteOnce claim, or one not in room.yaml, pins the pods to one node",
				v.Name, v.PersistentVolumeClaim.ClaimName)
		}
	}

	server := pod.Containers[0]
	// The 2 s HTTP drain (web.Serve) and a 30 s mail send (sendMailTimeout) run after the sleep.
	if ps := server.Lifecycle.PreStop; ps == nil || ps.Sleep == nil || ps.Sleep.Seconds < 1 {
		t.Error("server container has no preStop sleep: the ingress still routes to a terminating pod (docs/rollout.md)")
	} else if g := pod.TerminationGracePeriodSeconds; g == nil || *g < ps.Sleep.Seconds+2+30 {
		t.Errorf("terminationGracePeriodSeconds unset or below preStop %d s + 2 s drain + 30 s mail", ps.Sleep.Seconds)
	}
	env := func(name string) (k8sEnv, bool) {
		i := slices.IndexFunc(server.Env, func(e k8sEnv) bool { return e.Name == name })
		if i < 0 {
			return k8sEnv{}, false
		}
		return server.Env[i], true
	}
	dsn, ok := env("GRILL_ROOM_DATABASE_URL")
	if !ok || !strings.HasPrefix(dsn.Value, "postgres://<K8S_PG_HOST>:<K8S_PG_PORT>/") || strings.Contains(dsn.Value, "@") {
		t.Errorf("GRILL_ROOM_DATABASE_URL %q: want postgres://${K8S_PG_HOST}:${K8S_PG_PORT}/..., no credentials", dsn.Value)
	}
	for name, key := range map[string]string{"PGUSER": "username", "PGPASSWORD": "password"} {
		e, _ := env(name)
		if ref := e.ValueFrom.SecretKeyRef; ref.Name != "<K8S_PG_SECRET>" || ref.Key != key {
			t.Errorf("%s from secret %q key %q, want ${K8S_PG_SECRET} key %s", name, ref.Name, ref.Key, key)
		}
	}

	toPG := false
	for _, np := range manifest[k8sNetPol](t, "netpol.yaml") {
		if np.Kind != "NetworkPolicy" || !np.Spec.PodSelector.matches(labels) || !slices.Contains(np.Spec.PolicyTypes, "Egress") {
			continue
		}
		for _, rule := range np.Spec.Egress {
			ns := slices.ContainsFunc(rule.To, func(p k8sPeer) bool {
				return p.NamespaceSelector != nil && p.PodSelector == nil &&
					p.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "<K8S_PG_NAMESPACE>"
			})
			port := slices.ContainsFunc(rule.Ports, func(p k8sPort) bool {
				return p.Port == "<K8S_PG_PORT>" && p.Protocol == "TCP"
			})
			toPG = toPG || ns && port
		}
	}
	if !toPG {
		t.Error("no NetworkPolicy lets the server pods reach ${K8S_PG_NAMESPACE} on TCP ${K8S_PG_PORT}")
	}
}
