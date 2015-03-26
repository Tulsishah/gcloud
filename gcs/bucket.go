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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/jacobsa/gcloud/gcs/httputil"
	"golang.org/x/net/context"
	"google.golang.org/api/googleapi"
	storagev1 "google.golang.org/api/storage/v1"
	"google.golang.org/cloud"
	"google.golang.org/cloud/storage"
)

// A request to create an object, accepted by Bucket.CreateObject.
type CreateObjectRequest struct {
	// Attributes with which the object should be created. The Name field must be
	// set; other zero-valued fields are ignored.
	//
	// Object names must:
	//
	// *  be non-empty.
	// *  be no longer than 1024 bytes.
	// *  be valid UTF-8.
	// *  not contain the code point U+000A (line feed).
	// *  not contain the code point U+000D (carriage return).
	//
	// See here for authoritative documentation:
	//     https://cloud.google.com/storage/docs/bucket-naming#objectnames
	Attrs storage.ObjectAttrs

	// A reader from which to obtain the contents of the object. Must be non-nil.
	Contents io.Reader

	// If non-nil, the object will be created/overwritten only if the current
	// generation for the object name is equal to the given value. Zero means the
	// object does not exist.
	GenerationPrecondition *int64
}

// A request to read the contents of an object at a particular generation.
type ReadObjectRequest struct {
	// The name of the object to read.
	Name string

	// The generation of the object to read. Zero means the latest generation.
	Generation int64
}

type StatObjectRequest struct {
	// The name of the object in question.
	Name string
}

// A request to update the metadata of an object, accepted by
// Bucket.UpdateObject.
type UpdateObjectRequest struct {
	// The name of the object to update. Must be specified.
	Name string

	// String fields in the object to update (or not). The semantics are as
	// follows, for a given field F:
	//
	//  *  If F is set to nil, the corresponding GCS object field is untouched.
	//
	//  *  If *F is the empty string, then the corresponding GCS object field is
	//     removed.
	//
	//  *  Otherwise, the corresponding GCS object field is set to *F.
	//
	//  *  There is no facility for setting a GCS object field to the empty
	//     string, since many of the fields do not actually allow that as a legal
	//     value.
	//
	// Note that the GCS object's content type field cannot be removed.
	ContentType     *string
	ContentEncoding *string
	ContentLanguage *string
	CacheControl    *string

	// User-provided metadata updates. Keys that are not mentioned are untouched.
	// Keys whose values are nil are deleted, and others are updated to the
	// supplied string. There is no facility for completely removing user
	// metadata.
	Metadata map[string]*string
}

// Bucket represents a GCS bucket, pre-bound with a bucket name and necessary
// authorization information.
//
// Each method that may block accepts a context object that is used for
// deadlines and cancellation. Users need not package authorization information
// into the context object (using cloud.WithContext or similar).
type Bucket interface {
	Name() string

	// List the objects in the bucket that meet the criteria defined by the
	// query, returning a result object that contains the results and potentially
	// a cursor for retrieving the next portion of the larger set of results.
	ListObjects(
		ctx context.Context,
		query *storage.Query) (*storage.Objects, error)

	// Create a reader for the contents of a particular generation of an object.
	// The caller must arrange for the reader to be closed when it is no longer
	// needed.
	//
	// If the object doesn't exist, err will be of type *NotFoundError.
	NewReader(
		ctx context.Context,
		req *ReadObjectRequest) (io.ReadCloser, error)

	// Create or overwrite an object according to the supplied request. The new
	// object is guaranteed to exist immediately for the purposes of reading (and
	// eventually for listing) after this method returns a nil error. It is
	// guaranteed not to exist before req.Contents returns io.EOF.
	//
	// If the request fails due to a precondition not being met, the error will
	// be of type *PreconditionError.
	CreateObject(
		ctx context.Context,
		req *CreateObjectRequest) (*storage.Object, error)

	// Return current information about the object with the given name.
	//
	// If the object doesn't exist, err will be of type *NotFoundError.
	StatObject(
		ctx context.Context,
		req *StatObjectRequest) (*storage.Object, error)

	// Update the object specified by newAttrs.Name, patching using the non-zero
	// fields of newAttrs.
	//
	// If the object doesn't exist, err will be of type *NotFoundError.
	UpdateObject(
		ctx context.Context,
		req *UpdateObjectRequest) (*storage.Object, error)

	// Delete the object with the given name.
	//
	// If the object doesn't exist, err will be of type *NotFoundError.
	DeleteObject(ctx context.Context, name string) error
}

type bucket struct {
	projID     string
	client     *http.Client
	rawService *storagev1.Service
	name       string
}

func (b *bucket) Name() string {
	return b.name
}

func (b *bucket) ListObjects(
	ctx context.Context,
	query *storage.Query) (*storage.Objects, error) {
	authContext := cloud.WithContext(ctx, b.projID, b.client)
	return storage.ListObjects(authContext, b.name, query)
}

func (b *bucket) NewReader(
	ctx context.Context,
	req *ReadObjectRequest) (rc io.ReadCloser, err error) {
	// Construct an appropriate URL.
	//
	// The documentation (http://goo.gl/gZo4oj) is extremely vague about how this
	// is supposed to work. As of 2015-03-13, it says only that "for most
	// operations" you can use the following form to access an object:
	//
	//     storage.googleapis.com/<bucket>/<object>
	//
	// In Google-internal bug 19718068, it was clarified that the intent is that
	// each of the bucket and object names are encoded into a single path
	// segment, as defined by RFC 3986.
	bucketSegment := httputil.EncodePathSegment(b.name)
	objectSegment := httputil.EncodePathSegment(req.Name)
	opaque := fmt.Sprintf(
		"//storage.googleapis.com/%s/%s",
		bucketSegment,
		objectSegment)

	url := &url.URL{
		Scheme: "https",
		Opaque: opaque,
	}

	// Add a generation condition, if specified.
	if req.Generation != 0 {
		url.RawQuery = fmt.Sprintf("generation=%v", req.Generation)
	}

	// Call the server.
	httpRes, err := b.client.Get(url.String())
	if err != nil {
		err = fmt.Errorf("Get: %v", err)
		return
	}

	// Check for HTTP error statuses.
	if err = googleapi.CheckResponse(httpRes); err != nil {
		googleapi.CloseBody(httpRes)

		// Special case: handle not found errors.
		if typed, ok := err.(*googleapi.Error); ok {
			if typed.Code == http.StatusNotFound {
				err = &NotFoundError{Err: typed}
			}
		}

		return
	}

	// The body contains the object data.
	rc = httpRes.Body

	return
}

func fromRawAcls(in []*storagev1.ObjectAccessControl) []storage.ACLRule {
	out := make([]storage.ACLRule, len(in))
	for i, rule := range in {
		out[i] = storage.ACLRule{
			Entity: storage.ACLEntity(rule.Entity),
			Role:   storage.ACLRole(rule.Role),
		}
	}

	return out
}

func fromRfc3339(s string) (t time.Time, err error) {
	if s != "" {
		t, err = time.Parse(time.RFC3339, s)
	}

	return
}

func fromRawObject(
	bucketName string,
	in *storagev1.Object) (out *storage.Object, err error) {
	// Convert the easy fields.
	out = &storage.Object{
		Bucket:          bucketName,
		Name:            in.Name,
		ContentType:     in.ContentType,
		ContentLanguage: in.ContentLanguage,
		CacheControl:    in.CacheControl,
		ACL:             fromRawAcls(in.Acl),
		ContentEncoding: in.ContentEncoding,
		Size:            int64(in.Size),
		MediaLink:       in.MediaLink,
		Metadata:        in.Metadata,
		Generation:      in.Generation,
		MetaGeneration:  in.Metageneration,
		StorageClass:    in.StorageClass,
	}

	// Handle special cases.
	if in.Owner != nil {
		out.Owner = in.Owner.Entity
	}

	if out.Deleted, err = fromRfc3339(in.TimeDeleted); err != nil {
		err = fmt.Errorf("Decoding TimeDeleted field: %v", err)
		return
	}

	if out.Updated, err = fromRfc3339(in.Updated); err != nil {
		err = fmt.Errorf("Decoding Updated field: %v", err)
		return
	}

	if out.MD5, err = base64.StdEncoding.DecodeString(in.Md5Hash); err != nil {
		err = fmt.Errorf("Decoding Md5Hash field: %v", err)
		return
	}

	crc32cString, err := base64.StdEncoding.DecodeString(in.Crc32c)
	if err != nil {
		err = fmt.Errorf("Decoding Crc32c field: %v", err)
		return
	}

	if len(crc32cString) != 4 {
		err = fmt.Errorf(
			"Wrong length for decoded Crc32c field: %d",
			len(crc32cString))

		return
	}

	out.CRC32C =
		uint32(crc32cString[0])<<24 |
			uint32(crc32cString[1])<<16 |
			uint32(crc32cString[2])<<8 |
			uint32(crc32cString[3])<<0

	return
}

func (b *bucket) CreateObject(
	ctx context.Context,
	req *CreateObjectRequest) (o *storage.Object, err error) {
	o, err = createObject(b.client, b.Name(), ctx, req)
	return
}

func (b *bucket) StatObject(
	ctx context.Context,
	req *StatObjectRequest) (o *storage.Object, err error) {
	authContext := cloud.WithContext(ctx, b.projID, b.client)
	o, err = storage.StatObject(authContext, b.name, req.Name)

	// Transform errors.
	if err == storage.ErrObjectNotExist {
		err = &NotFoundError{
			Err: err,
		}

		return
	}

	return
}

func (b *bucket) UpdateObject(
	ctx context.Context,
	req *UpdateObjectRequest) (o *storage.Object, err error) {
	// Set up a map representing the JSON object we want to send to GCS. For now,
	// we don't treat empty strings specially.
	jsonMap := make(map[string]interface{})

	if req.ContentType != nil {
		jsonMap["contentType"] = req.ContentType
	}

	if req.ContentEncoding != nil {
		jsonMap["contentEncoding"] = req.ContentEncoding
	}

	if req.ContentLanguage != nil {
		jsonMap["contentLanguage"] = req.ContentLanguage
	}

	if req.CacheControl != nil {
		jsonMap["cacheControl"] = req.CacheControl
	}

	// Implement the convention that a pointer to an empty string means to delete
	// the field (communicated to GCS by setting it to null in the JSON).
	for k, v := range jsonMap {
		if *(v.(*string)) == "" {
			jsonMap[k] = nil
		}
	}

	// Add a field for user metadata if appropriate.
	if req.Metadata != nil {
		jsonMap["metadata"] = req.Metadata
	}

	// Set up a reader for the JSON object.
	body, err := googleapi.WithoutDataWrapper.JSONReader(jsonMap)
	if err != nil {
		return
	}

	// Set up URL params.
	urlParams := make(url.Values)
	urlParams.Set("projection", "full")

	// Set up the URL with a tempalte that we will later expand.
	url := googleapi.ResolveRelative(
		b.rawService.BasePath,
		"b/{bucket}/o/{object}")

	url += "?" + urlParams.Encode()

	// Create an HTTP request using NewRequest, which parses the URL string.
	// Expand the URL object it creates.
	httpReq, err := http.NewRequest("PATCH", url, body)
	if err != nil {
		err = fmt.Errorf("http.NewRequest: %v", err)
		return
	}

	googleapi.Expand(
		httpReq.URL,
		map[string]string{
			"bucket": b.Name(),
			"object": req.Name,
		})

	// Set up HTTP request headers.
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "github.com-jacobsa-gloud-gcs")

	// Execute the HTTP request.
	httpRes, err := b.client.Do(httpReq)
	if err != nil {
		return
	}

	defer googleapi.CloseBody(httpRes)

	if err = googleapi.CheckResponse(httpRes); err != nil {
		// Special case: handle not found errors.
		if typed, ok := err.(*googleapi.Error); ok {
			if typed.Code == http.StatusNotFound {
				err = &NotFoundError{Err: typed}
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
	if o, err = fromRawObject(b.Name(), rawObject); err != nil {
		return
	}

	return
}

func (b *bucket) DeleteObject(ctx context.Context, name string) (err error) {
	// Call the wrapped package.
	authContext := cloud.WithContext(ctx, b.projID, b.client)
	err = storage.DeleteObject(authContext, b.name, name)

	// Transform errors.
	if err == storage.ErrObjectNotExist {
		err = &NotFoundError{
			Err: err,
		}
	}

	// Handle the fact that as of 2015-03-12, the wrapped package does not
	// correctly return ErrObjectNotExist here.
	if typed, ok := err.(*googleapi.Error); ok {
		if typed.Code == http.StatusNotFound {
			err = &NotFoundError{Err: typed}
		}
	}

	return
}

func newBucket(
	projID string,
	client *http.Client,
	rawService *storagev1.Service,
	name string) Bucket {
	return &bucket{
		projID:     projID,
		client:     client,
		rawService: rawService,
		name:       name,
	}
}
