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

package regulator

import (
	"fmt"
	"git.torproject.org/pluggable-transports/goptlib.git"
	"github.com/websitefingerprinting/wfdef.git/transports/defconn"
	"strconv"
)

type jsonServerState struct {
	defconn.JsonServerState
	R int32   `json:"r"`
	D float32 `json:"d"`
	T float32 `json:"t"`
	N int32   `json:"n"`
	U float32 `json:"u"`
	C float32 `json:"c"`
}

type regulatorServerState struct {
	defconn.DefConnServerState
	r int32
	d float32
	t float32
	n int32
	u float32
	c float32
}

func (st *regulatorServerState) clientString() string {
	return st.DefConnServerState.ClientString() +
		fmt.Sprintf("%s=%d %s=%.2f %s=%.2f %s=%d %s=%.2f %s=%.2f",
			rArg, st.r, dArg, st.d, tArg, st.t, nArg, st.n, uArg, st.u, cArg, st.c)
}

func serverStateFromArgs(stateDir string, args *pt.Args) (*regulatorServerState, error) {
	js, err := defconn.ServerStateFromArgsInternal(stateDir, defconn.StateFile, args)
	if err != nil {
		return nil, err
	}

	rStr, rOk := args.Get(rArg)
	dStr, dOk := args.Get(dArg)
	tStr, tOk := args.Get(tArg)
	nStr, nOk := args.Get(nArg)
	uStr, uOk := args.Get(uArg)
	cStr, cOk := args.Get(cArg)

	var jsRegulator jsonServerState
	jsRegulator.JsonServerState = js

	// The regulator params should be independently configurable.
	if rOk {
		r, err := strconv.ParseInt(rStr, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed r '%s'", rStr)
		}
		jsRegulator.R = int32(r)
	} else {
		return nil, fmt.Errorf("missing argument '%s'", rArg)
	}

	if dOk {
		d, err := strconv.ParseFloat(dStr, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed d '%s'", dStr)
		}
		jsRegulator.D = float32(d)
	} else {
		return nil, fmt.Errorf("missing argument '%s'", dArg)
	}

	if tOk {
		t, err := strconv.ParseFloat(tStr, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed t '%s'", tStr)
		}
		jsRegulator.T = float32(t)
	} else {
		return nil, fmt.Errorf("missing argument '%s'", tArg)
	}

	if nOk {
		n, err := strconv.ParseInt(nStr, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed n '%s'", nStr)
		}
		jsRegulator.N = int32(n)
	} else {
		return nil, fmt.Errorf("missing argument '%s'", nArg)
	}

	if uOk {
		u, err := strconv.ParseFloat(uStr, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed u '%s'", uStr)
		}
		jsRegulator.U = float32(u)
	} else {
		return nil, fmt.Errorf("missing argument '%s'", uArg)
	}

	if cOk {
		c, err := strconv.ParseFloat(cStr, 32)
		if err != nil {
			return nil, fmt.Errorf("malformed c '%s'", cStr)
		}
		jsRegulator.C = float32(c)
	} else {
		return nil, fmt.Errorf("missing argument '%s'", cArg)
	}

	return serverStateFromJSONServerState(stateDir, &jsRegulator)
}

func serverStateFromJSONServerState(stateDir string, js *jsonServerState) (*regulatorServerState, error) {
	st, err := defconn.ServerStateFromJsonServerStateInternal(js)

	if js.R <= 0 || js.N <= 0 {
		return nil, fmt.Errorf("invalid R '%d' or N '%d'", js.R, js.N)
	}
	if js.D <= 0 || js.T <= 0 || js.U <= 0 || js.C <= 0 {
		return nil, fmt.Errorf("invalid d '%f' or t '%f' or u '%f' or c '%f'", js.D, js.T, js.U, js.C)
	}

	var stRegulator regulatorServerState

	stRegulator.DefConnServerState = st

	stRegulator.r = js.R
	stRegulator.d = js.D
	stRegulator.t = js.T
	stRegulator.n = js.N
	stRegulator.u = js.U
	stRegulator.c = js.C

	// Generate a human readable summary of the configured endpoint.
	if err = defconn.NewBridgeFile(stateDir, defconn.BridgeFile, stRegulator.clientString()); err != nil {
		return nil, err
	}

	// Write back the possibly updated server state.
	return &stRegulator, defconn.WriteJSONServerState(stateDir, defconn.StateFile, js)
}
