// Copyright 2015 Google Inc. All Rights Reserved.
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

package gcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"unicode/utf8"

	"github.com/jacobsa/gcloud/httputil"
	"golang.org/x/net/context"
	"google.golang.org/api/googleapi"
	storagev1 "google.golang.org/api/storage/v1"
)

func toRawObject(
	bucketName string,
	in *CreateObjectRequest) (out *storagev1.Object, err error) {
	out = &storagev1.Object{
		Bucket:          bucketName,
		Name:            in.Name,
		ContentType:     in.ContentType,
		ContentLanguage: in.ContentLanguage,
		ContentEncoding: in.ContentEncoding,
		CacheControl:    in.CacheControl,
		Metadata:        in.Metadata,
	}

	return
}

// Create the JSON for an "object resource", for use in an Objects.insert body.
func serializeMetadata(
	bucketName string,
	req *CreateObjectRequest) (out []byte, err error) {
	// Convert to storagev1.Object.
	rawObject, err := toRawObject(bucketName, req)
	if err != nil {
		err = fmt.Errorf("toRawObject: %v", err)
		return
	}

	// Serialize.
	out, err = json.Marshal(rawObject)
	if err != nil {
		err = fmt.Errorf("json.Marshal: %v", err)
		return
	}

	return
}

func startResumableUpload(
	httpClient *http.Client,
	bucketName string,
	ctx context.Context,
	req *CreateObjectRequest) (uploadURL string, err error) {
	// Construct an appropriate URL.
	//
	// The documentation (http://goo.gl/IJSlVK) is extremely vague about how this
	// is supposed to work. As of 2015-03-26, it simply gives an example:
	//
	//     POST https://www.googleapis.com/upload/storage/v1/b/<bucket>/o
	//
	// In Google-internal bug 19718068, it was clarified that the intent is that
	// the bucket name be encoded into a single path segment, as defined by RFC
	// 3986.
	bucketSegment := httputil.EncodePathSegment(bucketName)
	opaque := fmt.Sprintf(
		"//www.googleapis.com/upload/storage/v1/b/%s/o",
		bucketSegment)

	url := &url.URL{
		Scheme:   "https",
		Opaque:   opaque,
		RawQuery: "uploadType=resumable&projection=full",
	}

	if req.GenerationPrecondition != nil {
		url.RawQuery = fmt.Sprintf(
			"%s&ifGenerationMatch=%v",
			url.RawQuery,
			*req.GenerationPrecondition)
	}

	// Serialize the object metadata to JSON, for the request body.
	metadataJson, err := serializeMetadata(bucketName, req)
	if err != nil {
		err = fmt.Errorf("serializeMetadata: %v", err)
		return
	}

	// Create the HTTP request.
	httpReq, err := httputil.NewRequest(
		"POST",
		url,
		bytes.NewReader(metadataJson),
		userAgent)

	if err != nil {
		err = fmt.Errorf("httputil.NewRequest: %v", err)
		return
	}

	// Set up HTTP request headers.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Upload-Content-Type", req.ContentType)

	// Execute the HTTP request.
	httpRes, err := httpClient.Do(httpReq)
	if err != nil {
		return
	}

	defer googleapi.CloseBody(httpRes)

	// Check for HTTP-level errors.
	if err = googleapi.CheckResponse(httpRes); err != nil {
		return
	}

	// Extract the Location header.
	uploadURL = httpRes.Header.Get("Location")
	if uploadURL == "" {
		err = fmt.Errorf("Expected a Location header.")
		return
	}

	return
}

func createObject(
	httpClient *http.Client,
	bucketName string,
	ctx context.Context,
	req *CreateObjectRequest) (o *Object, err error) {
	// We encode using json.NewEncoder, which is documented to silently transform
	// invalid UTF-8 (cf. http://goo.gl/3gIUQB). So we can't rely on the server
	// to detect this for us.
	if !utf8.ValidString(req.Name) {
		err = errors.New("Invalid object name: not valid UTF-8")
		return
	}

	// Start a resumable upload, obtaining an upload URL.
	uploadURL, err := startResumableUpload(
		httpClient,
		bucketName,
		ctx,
		req)

	if err != nil {
		return
	}

	// Set up a follow-up request to the upload URL.
	httpReq, err := httputil.NewRequest("PUT", uploadURL, req.Contents, userAgent)
	if err != nil {
		err = fmt.Errorf("httputil.NewRequest: %v", err)
		return
	}

	httpReq.Header.Set("Content-Type", req.ContentType)

	// Execute the request.
	httpRes, err := httpClient.Do(httpReq)
	if err != nil {
		return
	}

	defer googleapi.CloseBody(httpRes)

	// Check for HTTP-level errors.
	if err = googleapi.CheckResponse(httpRes); err != nil {
		// Special case: handle precondition errors.
		if typed, ok := err.(*googleapi.Error); ok {
			if typed.Code == http.StatusPreconditionFailed {
				err = &PreconditionError{Err: typed}
			}
		}

		return
	}

	// Parse the response.
	var rawObject *storagev1.Object
	if err = json.NewDecoder(httpRes.Body).Decode(&rawObject); err != nil {
		return
	}

	// Convert the response.
	if o, err = toObject(rawObject); err != nil {
		err = fmt.Errorf("toObject: %v", err)
		return
	}

	return
}
