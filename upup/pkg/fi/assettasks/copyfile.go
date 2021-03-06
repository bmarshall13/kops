/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package assettasks

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/golang/glog"
	"k8s.io/kops/pkg/acls"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/util/pkg/hashing"
	"k8s.io/kops/util/pkg/vfs"
)

// CopyFile copies an from a source file repository, to a target repository,
// typically used for highly secure clusters.
//go:generate fitask -type=CopyFile
type CopyFile struct {
	Name       *string
	SourceFile *string
	TargetFile *string
	SHA        *string
	Lifecycle  *fi.Lifecycle
}

var _ fi.CompareWithID = &CopyFile{}

func (e *CopyFile) CompareWithID() *string {
	// or should this be the SHA?
	return e.Name
}

// Find attempts to find a file.
func (e *CopyFile) Find(c *fi.Context) (*CopyFile, error) {

	targetSHAFile := fi.StringValue(e.TargetFile) + ".sha1"
	targetSHABytes, err := vfs.Context.ReadFile(targetSHAFile)
	if err != nil {
		if os.IsNotExist(err) {
			glog.V(4).Infof("unable to download: %q, assuming target file is not present, and if not present may not be an error: %v",
				targetSHAFile, err)
			return nil, nil
		} else {
			glog.V(4).Infof("unable to download: %q, %v", targetSHAFile, err)
			// TODO should we throw err here?
			return nil, nil
		}
	}

	targetSHA := string(targetSHABytes)
	if strings.TrimSpace(targetSHA) == strings.TrimSpace(fi.StringValue(e.SHA)) {
		actual := &CopyFile{
			Name:       e.Name,
			TargetFile: e.TargetFile,
			SHA:        e.SHA,
			SourceFile: e.SourceFile,
			Lifecycle:  e.Lifecycle,
		}
		glog.V(8).Infof("found matching target sha1 for file: %q", fi.StringValue(e.TargetFile))
		return actual, nil
	}

	glog.V(8).Infof("did not find same file, found mismatching target sha1 for file: %q", fi.StringValue(e.TargetFile))
	return nil, nil

}

// Run is the default run method.
func (e *CopyFile) Run(c *fi.Context) error {
	return fi.DefaultDeltaRunMethod(e, c)
}

func (s *CopyFile) CheckChanges(a, e, changes *CopyFile) error {
	if fi.StringValue(e.Name) == "" {
		return fi.RequiredField("Name")
	}
	if fi.StringValue(e.SourceFile) == "" {
		return fi.RequiredField("SourceFile")
	}
	if fi.StringValue(e.TargetFile) == "" {
		return fi.RequiredField("TargetFile")
	}
	return nil
}

func (_ *CopyFile) Render(c *fi.Context, a, e, changes *CopyFile) error {

	source := fi.StringValue(e.SourceFile)
	target := fi.StringValue(e.TargetFile)
	sourceSha := fi.StringValue(e.SHA)

	glog.V(2).Infof("copying bits from %q to %q", source, target)

	if err := transferFile(c, source, target, sourceSha); err != nil {
		return fmt.Errorf("unable to transfer %q to %q: %v", source, target, err)
	}

	return nil
}

// transferFile downloads a file from the source location, validates the file matches the SHA,
// and uploads the file to the target location.
func transferFile(c *fi.Context, source string, target string, sha string) error {

	// TODO drop file to disk, as vfs reads file into memory.  We load kubelet into memory for instance.
	// TODO in s3 can we do a copy file ... would need to test

	data, err := vfs.Context.ReadFile(source)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file not found %q: %v", source, err)
		}

		return fmt.Errorf("error downloading file %q: %v", source, err)
	}

	objectStore, err := buildVFSPath(target)
	if err != nil {
		return err
	}

	uploadVFS, err := vfs.Context.BuildVfsPath(objectStore)
	if err != nil {
		return fmt.Errorf("error building path %q: %v", objectStore, err)
	}

	shaTarget := objectStore + ".sha1"
	shaVFS, err := vfs.Context.BuildVfsPath(shaTarget)
	if err != nil {
		return fmt.Errorf("error building path %q: %v", shaTarget, err)
	}

	in := bytes.NewReader(data)
	dataHash, err := hashing.HashAlgorithmSHA1.Hash(in)
	if err != nil {
		return fmt.Errorf("unable to parse sha from file %q downloaded: %v", sha, err)
	}

	shaHash, err := hashing.FromString(strings.TrimSpace(sha))
	if err != nil {
		return fmt.Errorf("unable to hash sha: %q, %v", sha, err)
	}

	if !shaHash.Equal(dataHash) {
		return fmt.Errorf("the sha value in %q does not match %q calculated value %q", shaTarget, source, dataHash.String())
	}

	glog.Infof("uploading %q to %q", source, objectStore)
	if err := writeFile(c, uploadVFS, data); err != nil {
		return err
	}

	b := []byte(shaHash.Hex())
	if err := writeFile(c, shaVFS, b); err != nil {
		return err
	}

	return nil
}

func writeFile(c *fi.Context, p vfs.Path, data []byte) error {

	acl, err := acls.GetACL(p, c.Cluster)
	if err != nil {
		return err
	}

	if err = p.WriteFile(data, acl); err != nil {
		return fmt.Errorf("error writing path %v: %v", p, err)
	}

	return nil
}

// buildVFSPath task a recognizable https url and transforms that URL into the equivalent url with the the object
// store prefix.
func buildVFSPath(target string) (string, error) {
	if !strings.Contains(target, "://") || strings.HasPrefix(target, "memfs://") {
		return target, nil
	}

	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("unable to parse url: %q", target)
	}

	var vfsPath string

	// These matches only cover a subset of the URLs that you can use, but I am uncertain how to cover more of the possible
	// options.
	// This code parses the HOST and determines to use s3 or gs.
	// URLs.  For instance you can have the bucket name in the s3 url hostname.
	// We are translating known https urls such as https://s3.amazonaws.com/example-kops to vfs path like
	// s3://example-kops
	if u.Host == "s3.amazonaws.com" {
		vfsPath = "s3:/" + u.Path
	} else if u.Host == "storage.googleapis.com" {
		vfsPath = "gs:/" + u.Path
	} else {
		glog.Errorf("unable to determine vfs path s3, google storage, and file paths are supported")
		glog.Errorf("URLs starting with https://s3.amazonaws.com and http://storage.googleapis.com are transformed into s3 and gs URLs")
		return "", fmt.Errorf("unable to determine vfs type for %q", target)
	}

	return vfsPath, nil
}
