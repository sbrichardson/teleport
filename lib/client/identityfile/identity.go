/*
Copyright 2020 Gravitational, Inc.

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

// Package identityfile handles formatting and parsing of identity files.
package identityfile

import (
	"bufio"
	"bytes"
	"io"
	"io/ioutil"
	"os"

	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/sshutils"

	"github.com/gravitational/trace"
)

// Format describes possible file formats how a user identity can be stored.
type Format string

const (
	// FormatFile is when a key + cert are stored concatenated into a single file
	FormatFile Format = "file"

	// FormatOpenSSH is OpenSSH-compatible format, when a key and a cert are stored in
	// two different files (in the same directory)
	FormatOpenSSH Format = "openssh"

	// FormatTLS is a standard TLS format used by common TLS clients (e.g. GRPC) where
	// certificate and key are stored in separate files.
	FormatTLS Format = "tls"

	// DefaultFormat is what Teleport uses by default
	DefaultFormat = FormatFile
)

// Write takes a username + their credentials and saves them to disk
// in a specified format.
//
// filePath is used as a base to generate output file names; these names are
// returned in filesWritten.
func Write(filePath string, key *client.Key, format Format, certAuthorities []services.CertAuthority) (filesWritten []string, err error) {
	const (
		// the files and the dir will be created with these permissions:
		fileMode = 0600
		dirMode  = 0700
	)

	if filePath == "" {
		return nil, trace.BadParameter("identity location is not specified")
	}

	var output io.Writer = os.Stdout
	switch format {
	// dump user identity into a single file:
	case FormatFile:
		filesWritten = append(filesWritten, filePath)
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		output = f
		defer f.Close()

		// write key:
		if _, err = output.Write(key.Priv); err != nil {
			return nil, trace.Wrap(err)
		}
		// append ssh cert:
		if _, err = output.Write(key.Cert); err != nil {
			return nil, trace.Wrap(err)
		}
		// append tls cert:
		if _, err = output.Write(key.TLSCert); err != nil {
			return nil, trace.Wrap(err)
		}
		// append trusted host certificate authorities
		for _, ca := range certAuthorities {
			// append ssh ca certificates
			for _, publicKey := range ca.GetCheckingKeys() {
				data, err := sshutils.MarshalAuthorizedHostsFormat(ca.GetClusterName(), publicKey, nil)
				if err != nil {
					return nil, trace.Wrap(err)
				}
				if _, err = output.Write([]byte(data)); err != nil {
					return nil, trace.Wrap(err)
				}
				if _, err = output.Write([]byte("\n")); err != nil {
					return nil, trace.Wrap(err)
				}
			}
			// append tls ca certificates
			for _, keyPair := range ca.GetTLSKeyPairs() {
				if _, err = output.Write(keyPair.Cert); err != nil {
					return nil, trace.Wrap(err)
				}
			}
		}

	// dump user identity into separate files:
	case FormatOpenSSH:
		keyPath := filePath
		certPath := keyPath + "-cert.pub"
		filesWritten = append(filesWritten, keyPath, certPath)

		err = ioutil.WriteFile(certPath, key.Cert, fileMode)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		err = ioutil.WriteFile(keyPath, key.Priv, fileMode)
		if err != nil {
			return nil, trace.Wrap(err)
		}

	case FormatTLS:
		keyPath := filePath + ".key"
		certPath := filePath + ".crt"
		casPath := filePath + ".cas"
		filesWritten = append(filesWritten, keyPath, certPath, casPath)

		err = ioutil.WriteFile(certPath, key.TLSCert, fileMode)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		err = ioutil.WriteFile(keyPath, key.Priv, fileMode)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		var caCerts []byte
		for _, ca := range certAuthorities {
			for _, keyPair := range ca.GetTLSKeyPairs() {
				caCerts = append(caCerts, keyPair.Cert...)
			}
		}
		err = ioutil.WriteFile(casPath, caCerts, fileMode)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	default:
		return nil, trace.BadParameter("unsupported identity format: %q, use one of %q, %q, or %q",
			format, FormatFile, FormatOpenSSH, FormatTLS)
	}
	return filesWritten, nil
}

// IdentityFile represents the basic components of an identity file.
type IdentityFile struct {
	PrivateKey []byte
	Certs      struct {
		SSH []byte
		TLS []byte
	}
	CACerts struct {
		SSH [][]byte
		TLS [][]byte
	}
}

// Decode attempts to break up the contents of an identity file
// into its respective components.
func Decode(r io.Reader) (*IdentityFile, error) {
	scanner := bufio.NewScanner(r)
	var ident IdentityFile
	// Subslice of scanner's buffer pointing to current line
	// with leading and trailing whitespace trimmed.
	var line []byte
	// Attempt to scan to the next line.
	scanln := func() bool {
		if !scanner.Scan() {
			line = nil
			return false
		}
		line = bytes.TrimSpace(scanner.Bytes())
		return true
	}
	// Check if the current line starts with prefix `p`.
	peekln := func(p string) bool {
		return bytes.HasPrefix(line, []byte(p))
	}
	// Get an "owned" copy of the current line.
	cloneln := func() []byte {
		ln := make([]byte, len(line))
		copy(ln, line)
		return ln
	}
	// Scan through all lines of identity file.  Lines with a known prefix
	// are copied out of the scanner's buffer.  All others are ignored.
	for scanln() {
		switch {
		case peekln("ssh"):
			ident.Certs.SSH = cloneln()
		case peekln("@cert-authority"):
			ident.CACerts.SSH = append(ident.CACerts.SSH, cloneln())
		case peekln("-----BEGIN"):
			// Current line marks the beginning of a PEM block.  Consume all
			// lines until a corresponding END is found.
			var pemBlock []byte
			for {
				pemBlock = append(pemBlock, line...)
				pemBlock = append(pemBlock, '\n')
				if peekln("-----END") {
					break
				}
				if !scanln() {
					// If scanner has terminated in the middle of a PEM block, either
					// the reader encountered an error, or the PEM block is a fragment.
					if err := scanner.Err(); err != nil {
						return nil, trace.Wrap(err)
					}
					return nil, trace.BadParameter("invalid PEM block (fragment)")
				}
			}
			// Decide where to place the pem block based on
			// which pem blocks have already been found.
			switch {
			case ident.PrivateKey == nil:
				ident.PrivateKey = pemBlock
			case ident.Certs.TLS == nil:
				ident.Certs.TLS = pemBlock
			default:
				ident.CACerts.TLS = append(ident.CACerts.TLS, pemBlock)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, trace.Wrap(err)
	}
	return &ident, nil
}
