// Copyright 2024 The Tessera authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// example-gcp is a simple personality showing how to use the Tessera GCP storage
// implmentation.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	tessera "github.com/transparency-dev/trillian-tessera"
	"github.com/transparency-dev/trillian-tessera/storage/gcp"
	"golang.org/x/mod/sumdb/note"
	"k8s.io/klog/v2"
)

var (
	bucket  = flag.String("bucket", "", "Bucket to use for storing log")
	listen  = flag.String("listen", ":2024", "Address:port to listen on")
	project = flag.String("project", os.Getenv("GOOGLE_CLOUD_PROJECT"), "GCP Project, take from env if unset")
	spanner = flag.String("spanner", "", "Spanner resource URI ('projects/.../...')")
	signer  = flag.String("signer", "", "Path to file containing log private key")
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	ctx := context.Background()

	gcpCfg := gcp.Config{
		ProjectID: *project,
		Bucket:    *bucket,
		Spanner:   *spanner,
	}
	storage, err := gcp.New(ctx, gcpCfg,
		tessera.WithCheckpointSignerVerifier(signerFromFlags(), nil),
		tessera.WithBatching(1024, time.Second),
		tessera.WithPushback(10*4096),
	)
	if err != nil {
		klog.Exitf("Failed to create new GCP storage: %v", err)
	}

	http.HandleFunc("POST /add", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()

		id := sha256.Sum256(b)
		idx, err := storage.Add(r.Context(), tessera.NewEntry(b, tessera.WithIdentity(id[:])))
		if err != nil {
			if errors.Is(err, tessera.ErrPushback) {
				w.Header().Add("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf("%d", idx)))
	})

	// TODO: remove this proxy
	serveGCS := func(w http.ResponseWriter, r *http.Request) {
		resource := strings.TrimLeft(r.URL.Path, "/")
		b, err := storage.Get(r.Context(), resource)
		if err != nil {
			klog.V(1).Infof("Get: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(fmt.Sprintf("Get: %v", err)))
			return
		}
		_, _ = w.Write(b)
	}
	http.HandleFunc("GET /checkpoint", serveGCS)
	http.HandleFunc("GET /tile/", serveGCS)

	if err := http.ListenAndServe(*listen, http.DefaultServeMux); err != nil {
		klog.Exitf("ListenAndServe: %v", err)
	}
}

func signerFromFlags() note.Signer {
	raw, err := os.ReadFile(*signer)
	if err != nil {
		klog.Exitf("Failed to read secret key file %q: %v", *signer, err)
	}
	signer, err := note.NewSigner(string(raw))
	if err != nil {
		klog.Exitf("Failed to create new signer: %v", err)
	}
	return signer
}
