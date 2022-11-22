/*
 * Copyright (c) 2014, Yawning Angel <yawning at schwanenlied dot me>
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *  * Redistributions of source code must retain the above copyright notice,
 *    this list of conditions and the following disclaimer.
 *
 *  * Redistributions in binary form must reproduce the above copyright notice,
 *    this list of conditions and the following disclaimer in the documentation
 *    and/or other materials provided with the distribution.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 * AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
 * ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
 * LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
 * CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
 * SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
 * INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
 * CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
 * ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGE.
 */

package wfgan

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"git.torproject.org/pluggable-transports/goptlib.git"
	"github.com/websitefingerprinting/wfdef.git/common/csrand"
	"github.com/websitefingerprinting/wfdef.git/common/drbg"
	"github.com/websitefingerprinting/wfdef.git/common/ntor"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
)

const (
	stateFile  = "wfgan_state.json"
	bridgeFile = "wfgan_bridgeline.txt"

	certSuffix = "=="
	certLength = ntor.NodeIDLength + ntor.PublicKeyLength
)

type jsonServerState struct {
	NodeID     string         `json:"node-id"`
	PrivateKey string         `json:"private-key"`
	PublicKey  string         `json:"public-key"`
	DrbgSeed   string         `json:"drbg-seed"`
	Tol        float32        `json:"tol"`
	P          float32        `json:"p"`
}

type wfganServerCert struct {
	raw []byte
}

func (cert *wfganServerCert) String() string {
	return strings.TrimSuffix(base64.StdEncoding.EncodeToString(cert.raw), certSuffix)
}

func (cert *wfganServerCert) unpack() (*ntor.NodeID, *ntor.PublicKey) {
	if len(cert.raw) != certLength {
		panic(fmt.Sprintf("cert length %d is invalid", len(cert.raw)))
	}

	nodeID, _ := ntor.NewNodeID(cert.raw[:ntor.NodeIDLength])
	pubKey, _ := ntor.NewPublicKey(cert.raw[ntor.NodeIDLength:])

	return nodeID, pubKey
}

func serverCertFromString(encoded string) (*wfganServerCert, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded + certSuffix)
	if err != nil {
		return nil, fmt.Errorf("failed to decode cert: %s", err)
	}

	if len(decoded) != certLength {
		return nil, fmt.Errorf("cert length %d is invalid", len(decoded))
	}

	return &wfganServerCert{raw: decoded}, nil
}

func serverCertFromState(st *wfganServerState) *wfganServerCert {
	cert := new(wfganServerCert)
	cert.raw = append(st.nodeID.Bytes()[:], st.identityKey.Public().Bytes()[:]...)
	return cert
}

type wfganServerState struct {
	nodeID      *ntor.NodeID
	identityKey *ntor.Keypair
	drbgSeed    *drbg.Seed
	tol         float32
	p         float32
	cert        *wfganServerCert
}

func (st *wfganServerState) clientString() string {
	return fmt.Sprintf("%s=%s %s=%.1f %s=%.1f", certArg, st.cert, tolArg, st.tol, pArg, st.p)
}

func serverStateFromArgs(stateDir string, args *pt.Args) (*wfganServerState, error) {
	var js jsonServerState
	var nodeIDOk, privKeyOk, seedOk bool
	js.NodeID, nodeIDOk = args.Get(nodeIDArg)
	js.PrivateKey, privKeyOk = args.Get(privateKeyArg)
	js.DrbgSeed, seedOk = args.Get(seedArg)
	tolStr, tolOk := args.Get(tolArg)
	pStr, pOk := args.Get(pArg)

	// Either a private key, node id, and seed are ALL specified, or
	// they should be loaded from the state file.
	if !privKeyOk && !nodeIDOk && !seedOk {
		if err := jsonServerStateFromFile(stateDir, &js); err != nil {
			return nil, err
		}
	} else if !privKeyOk {
		return nil, fmt.Errorf("missing argument '%s'", privateKeyArg)
	} else if !nodeIDOk {
		return nil, fmt.Errorf("missing argument '%s'", nodeIDArg)
	} else if !seedOk {
		return nil, fmt.Errorf("missing argument '%s'", seedArg)
	}

	// The wfgan params should be independently configurable.
	if tolOk {
		tol, err := strconv.ParseFloat(tolStr, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed tol '%s'", tolStr)
		}
		js.Tol = float32(tol)
	} else {
		return nil, fmt.Errorf("missing argument '%s'", tolArg)
	}
	if pOk {
		p, err := strconv.ParseFloat(pStr, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed p '%s'", pStr)
		}
		js.P = float32(p)
	} else {
		return nil, fmt.Errorf("missing argument '%s'", pArg)
	}
	return serverStateFromJSONServerState(stateDir, &js)
}

func serverStateFromJSONServerState(stateDir string, js *jsonServerState) (*wfganServerState, error) {
	var err error

	st := new(wfganServerState)
	if st.nodeID, err = ntor.NodeIDFromHex(js.NodeID); err != nil {
		return nil, err
	}
	if st.identityKey, err = ntor.KeypairFromHex(js.PrivateKey); err != nil {
		return nil, err
	}
	if st.drbgSeed, err = drbg.SeedFromHex(js.DrbgSeed); err != nil {
		return nil, err
	}
	if js.Tol < 0{
		return nil, fmt.Errorf("invalid tol '%.1f'", js.Tol)
	}
	if js.P < 0{
		return nil, fmt.Errorf("invalid p '%.1f'", js.P)
	}

	st.tol = js.Tol
	st.p = js.P

	st.cert = serverCertFromState(st)

	// Generate a human readable summary of the configured endpoint.
	if err = newBridgeFile(stateDir, st); err != nil {
		return nil, err
	}

	// Write back the possibly updated server state.
	return st, writeJSONServerState(stateDir, js)
}

func jsonServerStateFromFile(stateDir string, js *jsonServerState) error {
	fPath := path.Join(stateDir, stateFile)
	f, err := ioutil.ReadFile(fPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err = newJSONServerState(stateDir, js); err == nil {
				return nil
			}
		}
		return err
	}

	if err := json.Unmarshal(f, js); err != nil {
		return fmt.Errorf("failed to load statefile '%s': %s", fPath, err)
	}

	return nil
}

func newJSONServerState(stateDir string, js *jsonServerState) (err error) {
	// Generate everything a server needs, using the cryptographic PRNG.
	var st wfganServerState
	rawID := make([]byte, ntor.NodeIDLength)
	if err = csrand.Bytes(rawID); err != nil {
		return
	}
	if st.nodeID, err = ntor.NewNodeID(rawID); err != nil {
		return
	}
	if st.identityKey, err = ntor.NewKeypair(false); err != nil {
		return
	}
	if st.drbgSeed, err = drbg.NewSeed(); err != nil {
		return
	}

	// Encode it into JSON format and write the state file.
	js.NodeID = st.nodeID.Hex()
	js.PrivateKey = st.identityKey.Private().Hex()
	js.PublicKey = st.identityKey.Public().Hex()
	js.DrbgSeed = st.drbgSeed.Hex()

	return writeJSONServerState(stateDir, js)
}

func writeJSONServerState(stateDir string, js *jsonServerState) error {
	var err error
	var encoded []byte
	if encoded, err = json.Marshal(js); err != nil {
		return err
	}
	if err = ioutil.WriteFile(path.Join(stateDir, stateFile), encoded, 0600); err != nil {
		return err
	}

	return nil
}

func newBridgeFile(stateDir string, st *wfganServerState) error {
	const prefix = "# wfgan torrc client bridge line\n" +
		"#\n" +
		"# This file is an automatically generated bridge line based on\n" +
		"# the current wfganproxy configuration.  EDITING IT WILL HAVE\n" +
		"# NO EFFECT.\n" +
		"#\n" +
		"# Before distributing this Bridge, edit the placeholder fields\n" +
		"# to contain the actual values:\n" +
		"#  <IP ADDRESS>  - The public IP address of your wfgan bridge.\n" +
		"#  <PORT>        - The TCP/IP port of your wfgan bridge.\n" +
		"#  <FINGERPRINT> - The bridge's fingerprint.\n\n"

	bridgeLine := fmt.Sprintf("Bridge wfgan <IP ADDRESS>:<PORT> <FINGERPRINT> %s\n",
		st.clientString())

	tmp := []byte(prefix + bridgeLine)
	if err := ioutil.WriteFile(path.Join(stateDir, bridgeFile), tmp, 0600); err != nil {
		return err
	}

	return nil
}
