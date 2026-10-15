// Package devicestt pins the files a browser needs to transcribe on its own device (spec 010): the
// model, the onnxruntime-web runtime and the Silero VAD. The room server serves them from a model
// directory that `grill-room models fetch` fills.
package devicestt

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Archive is a pinned tarball (.tgz) some files are extracted from.
type Archive struct {
	URL    string
	Size   int64
	SHA256 string
}

// File is one pinned file, stored flat under the model directory by Name. It comes from URL, or
// from member Member of Archive.
type File struct {
	Name    string
	Size    int64
	SHA256  string
	URL     string
	Archive *Archive
	Member  string
}

// Manifest is the set of files device mode needs.
type Manifest struct {
	Files []File
}

const (
	hfModel   = "https://huggingface.co/Olicorne/parakeet-tdt-0.6b-v3-ultra-onnx/resolve/a1fe742758beb8ddc918bff00e1e319fe7e4d3e3/"
	sileroURL = "https://raw.githubusercontent.com/SYSTRAN/faster-whisper/65882eee9f5cdbeeb2d877f1131d48cf241b327d/faster_whisper/assets/silero_vad_v6.onnx"
)

var ortWeb = &Archive{
	URL:    "https://registry.npmjs.org/onnxruntime-web/-/onnxruntime-web-1.30.0.tgz",
	Size:   33106585,
	SHA256: "d2228df7e4616bc3348bf504ee888f3bec43789a273f0a63f3e68d203ce3bf71",
}

// Pinned is the manifest compiled into the binary (010-AC6).
var Pinned = Manifest{Files: []File{
	{Name: "encoder-model.w4a8.onnx", Size: 383461215, SHA256: "b08d6c8e4993d1b36cf78f7e7798b703c1605d7f7735855142538ebe76f6fe82",
		URL: hfModel + "w4a8/encoder-model.w4a8.onnx"},
	{Name: "decoder_joint-model.int8.onnx", Size: 18203490, SHA256: "667ffde2242a560923cba6bc7f69ef03d736f8a36ca9588f2e6f9fa818cf2fc1",
		URL: hfModel + "int8/decoder_joint-model.int8.onnx"},
	{Name: "vocab.txt", Size: 93939, SHA256: "d58544679ea4bc6ac563d1f545eb7d474bd6cfa467f0a6e2c1dc1c7d37e3c35d",
		URL: hfModel + "vocab.txt"},
	{Name: "ort.all.min.mjs", Size: 819537, SHA256: "2e4a0eae53c3f246e26423aaca493d3bfc22ce524894f27519a9ec4a14e6397a",
		Archive: ortWeb, Member: "package/dist/ort.all.min.mjs"},
	{Name: "ort-wasm-simd-threaded.jsep.mjs", Size: 46851, SHA256: "709853412fd1ffc34247af1e73569227b5b79629c5ca3f59cc39cf7e500e4947",
		Archive: ortWeb, Member: "package/dist/ort-wasm-simd-threaded.jsep.mjs"},
	{Name: "ort-wasm-simd-threaded.jsep.wasm", Size: 28312028, SHA256: "3ad23231b5bd6d9dda55a7f84606315e0bf35b6750c28ee993c987c54cacab0f",
		Archive: ortWeb, Member: "package/dist/ort-wasm-simd-threaded.jsep.wasm"},
	{Name: "silero_vad_v6.onnx", Size: 1245151, SHA256: "4cbf549b8326f60f80f2536d9eefeb450a9abe83365a098031c89719f1be17d2",
		URL: sileroURL},
}}

// Revision names the manifest in /models/ URLs: browsers cache those forever, so any pin change
// must change it.
func (m Manifest) Revision() string {
	h := sha256.New()
	for _, f := range m.Files {
		src := f.URL
		if f.Archive != nil {
			src = fmt.Sprintf("%s %d %s %s", f.Archive.URL, f.Archive.Size, f.Archive.SHA256, f.Member)
		}
		_, _ = fmt.Fprintf(h, "%s %d %s %s\n", f.Name, f.Size, f.SHA256, src)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Lookup returns the file named name.
func (m Manifest) Lookup(name string) (File, bool) {
	for _, f := range m.Files {
		if f.Name == name {
			return f, true
		}
	}
	return File{}, false
}

// Integrity is the file's subresource integrity value, for fetch's integrity option (010-AC2).
func (f File) Integrity() string {
	sum, _ := hex.DecodeString(f.SHA256) // a pinned constant
	return "sha256-" + base64.StdEncoding.EncodeToString(sum)
}

// Check returns an error naming the first file of m missing from dir or failing its size or hash.
func (m Manifest) Check(dir string) error {
	for _, f := range m.Files {
		if err := f.check(dir); err != nil {
			return err
		}
	}
	return nil
}

func (f File) check(dir string) error {
	fh, err := os.Open(filepath.Join(dir, f.Name)) //nolint:gosec // the operator's directory, a pinned name
	if err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	defer fh.Close() //nolint:errcheck // read only
	if st, err := fh.Stat(); err != nil || st.Size() != f.Size || !st.Mode().IsRegular() {
		return fmt.Errorf("%s: want a regular file of %d bytes", f.Name, f.Size)
	}
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return fmt.Errorf("%s: %w", f.Name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%s: sha256 %s, want %s", f.Name, got, f.SHA256)
	}
	return nil
}

// Offer is what the room page learns of device mode in the stt event (010-AC5).
type Offer struct {
	Revision string      `json:"revision"`
	Bytes    int64       `json:"bytes"`
	Files    []OfferFile `json:"files"`
}

// OfferFile is one file the page downloads.
type OfferFile struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Integrity string `json:"integrity"`
}

// Offer describes m for the room page.
func (m Manifest) Offer() *Offer {
	o := &Offer{Revision: m.Revision(), Files: make([]OfferFile, 0, len(m.Files))}
	for _, f := range m.Files {
		o.Bytes += f.Size
		o.Files = append(o.Files, OfferFile{Name: f.Name, Size: f.Size, Integrity: f.Integrity()})
	}
	return o
}
