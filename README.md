# Surakav Implementation
<div >
<img src="https://img.shields.io/badge/Surakav-1.0.0-brightgreen.svg?logo=data:image/png;base64,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">
</div>

## What?
This repository extends WFDefProxy [5], a framework that has already implemented a bunch of defenses against Website Fingerprinting (WF) attacks, to implement our defense Surakav. 

WFDefProxy implements three defenses against Website Fingerprinting (WF) attack: FRONT [1], Tamaraw [2] and Random-WT [3]. 
It extends obfs4proxy [4], the state-of-the-art pluggable transport for Tor to circumvent censorship. 
It transforms the traffic between the client and the bridge according to a defense's protocol.
It makes use of the cryptographic system of obfs4 to do the handshake as well as to encrypt/decrypt the packets.

[comment]: <> (The workflow of WFDefProxy is shown in the figure below:)

[comment]: <> (<div  align="center"> )

[comment]: <> (<img src="https://anonymous.4open.science/r/wfdef-11EF/imgs/wfdefproxy.png" style="zoom:20%;" />)

[comment]: <> (</div>)

[//]: # (## Some Notes)

[//]: # (- This branch of repository is specially made for the double-blind review process. The package names are obfuscated in the code with the word `anonymous`.  )

[//]: # ()
[//]: # (- It is a fork of WFDefProxy, and we added our proposed WF defense Surakav on it. )

## Table of Contents
- [How to use?](#how-to-use-)
    * [To build:](#to-build-)
    * [To run **Surakav** (our defense)](#to-run---Surakav--) 
    * [To run **FRONT**](#to-run---front--)
    * [To run **Tamaraw**](#to-run---tamaraw--)
    * [To run **Random-WT**](#to-run---random-wt--)
- [How does WFDefProxy work?](#how-does-wfdefproxy-work-)
- [Tips and tricks](#tips-and-tricks)

<span id="how-to-use-">

## How to use? 

<span id="to-build-">

### To build:

```go build -o obfs4proxy/obfs4proxy ./obfs4proxy```

Suppose we put the compiled binary at `/Users/example/wfdef/obfs4proxy/obfs4proxy`.

</span>

<span id="to-run---Surakav--">

### To run Surakav (our defense)
The torrc configuration of bridge is like:
```
# Feel free to adapt the path.
DataDirectory /Users/example/tor-config/log-wfgan-server  
Log notice stdout    
SOCKSPort 9052    
AssumeReachable 1    
PublishServerDescriptor 0    
Exitpolicy reject *:*    
ORPort auto   
ExtORPort auto
Nickname "wfdef"    
BridgeRelay 1    
ServerTransportListenAddr wfgan 0.0.0.0:34000
ServerTransportPlugin wfgan exec /Users/example/wfdef/obfs4proxy/obfs4proxy
ServerTransportOptions wfgan tol=0.4
```
It will generate a `wfgan_bridgeline.txt` in `/Users/example/tor-config/log-wfgan-server/pt_state`,
containing a certification used for handshake as well as the configured parameters.

The client's torrc file is like:
```
DataDirectory /Users/example/tor-config/log-wfgan-client 
Log notice stdout    
SOCKSPort 9050  
ControlPort 9051  
UseBridges 1    
Bridge wfgan 127.0.0.1:34000 cert=VdXiHCbwjXAC3+M2VZwasp+TAIbK0TuQD3MG3s024pE3brEygUOovIJo4f2oxZpBvlrNFQ tol=0.4
ClientTransportPlugin wfgan exec /Users/example/wfdef/obfs4proxy/obfs4proxy
```

You can launch Tor with command line `tor -f client-torrc` or replace Tor Browser's torrc file with it and launch the Tor Browser directly.
Note that if is better to also include the relay's fingerprint in `Bridge` option due to some bugs of Tor Browser that may cause the launch failure.




</span>


<span id="to-run---front--">

### To run FRONT
The torrc configuration of bridge is like:
```
# Feel free to adapt the path.
DataDirectory /Users/example/tor-config/log-front-server  
Log notice stdout    
SOCKSPort 9052    
AssumeReachable 1    
PublishServerDescriptor 0    
Exitpolicy reject *:*    
ORPort auto   
ExtORPort auto
Nickname "wfdef"    
BridgeRelay 1    
ServerTransportListenAddr front 0.0.0.0:34000
ServerTransportPlugin front exec /Users/example/wfdef/obfs4proxy/obfs4proxy
ServerTransportOptions front w-min=1 w-max=13 n-client=3000 n-server=3000
```
It will generate a `front_bridgeline.txt` in `/Users/example/tor-config/log-front-server/pt_state`, 
containing a certification used for handshake as well as the configured parameters. 

The client's torrc file is like:
```
DataDirectory /Users/example/tor-config/log-front-client 
Log notice stdout    
SOCKSPort 9050  
ControlPort 9051  
UseBridges 1    
Bridge front 127.0.0.1:34000 cert=VdXiHCbwjXAC3+M2VZwasp+TAIbK0TuQD3MG3s024pE3brEygUOovIJo4f2oxZpBvlrNFQ w-min=1.0 w-max=13.0 n-server=3000 n-client=3000
ClientTransportPlugin front exec /Users/example/wfdef/obfs4proxy/obfs4proxy
```

You can launch Tor with command line `tor -f client-torrc` or replace Tor Browser's torrc file with it and launch the Tor Browser directly. 
Note that if is better to also include the relay's fingerprint in `Bridge` option due to some bugs of Tor Browser that may cause the launch failure.

</span>

<span id="to-run---tamaraw--">

### To run Tamaraw 
The torrc for bridge is similar as FRONT, except that last two lines should be 
```
ServerTransportPlugin tamaraw exec /Users/example/wfdef/obfs4proxy/obfs4proxy
ServerTransportOptions tamaraw rho-client=12 rho-server=4 nseg=200
```
Also, on the client side, the last two lines of the torrc file should be
```
Bridge tamaraw 127.0.0.1:34000 cert=VdXiHCbwjXAC3+M2VZwasp+TAIbK0TuQD3MG3s024pE3brEygUOovIJo4f2oxZpBvlrNFQ rho-client=12 rho-server=4 nseg=200
ClientTransportPlugin tamaraw exec /Users/example/wfdef/obfs4proxy/obfs4proxy
```
Replace `Bridge` with the information in `tamaraw_bridgeline.txt` in `/Users/example/tor-config/log-tamaraw-server/pt_state`.

</span>

<span id="to-run---random-wt--">

### To run Random-WT
The last two lines of torrc file for bridge:
```
ServerTransportPlugin randomwt exec /Users/example/wfdef/obfs4proxy/obfs4proxy
ServerTransportOptions randomwt n-client-real=4 n-server-real=45 n-client-fake=8 n-server-fake=90 p-fake=0.4
```
Similarly, the client side 
```
Bridge randomwt 127.0.0.1:34000 cert=VdXiHCbwjXAC3+M2VZwasp+TAIbK0TuQD3MG3s024pE3brEygUOovIJo4f2oxZpBvlrNFQ n-client-real=4 n-server-real=45 n-client-fake=8 n-server-fake=90 p-fake=0.4
ClientTransportPlugin randomwt exec /Users/example/wfdef/obfs4proxy/obfs4proxy
```
</span>

</span>

<span id="how-does-wfdefproxy-work-">

## How does WFDefProxy work?


See the original paper for the design details of WFDefProxy [5]. 

</span>


<span id="tips-and-tricks">

## Tips and tricks
* There are two ways to get the trace via WFDefProxy: 
  * The first one is to simply add some logs about the time and bytes of the packets sent or received near the `conn.Write` or `Read` function. I have written some. 
   The logs can be found at `/Users/example/tor-config/log-[defense]-client/pt_state/obfs4proxy.log`.
   Make sure the log function is enabled. 
   You can check `./obfs4proxy/obfs4proxy.go` Line 315-316 to enable and adjust the level of logging.
  * The second way is to enable `traceLogger` which is defined in the front of `[defense].go`. 
    Any outside programme can signal traceLogger to start/stop logging the packet information via gRPC communication.
    You should modify the following parameters in the code (provide an address and enable `traceLogger`):
    ```
    gRPCAddr        = "localhost:10086"
	traceLogEnabled    = true
    ```
    The definition of a gRPC message can be found at `./transports/pb/traceLog.proto`:
    ```
    message SignalMsg {
      bool turnOn = 1;
      string filePath = 2;
    }
    ```
    When received a message with `turnOn=true`, WFDefProxy will log the packet information (timestamp, direction and size) to `filePath`.
    When received a message with `turnOn=false`, WFDefProxy will stop logging.

* WFDefProxy can be used together with [WFCrawler](https://github.com/websitefingerprinting/WFCrawler), the toolkit we developed for crawling and parsing traces.

</span>

## Dependencies

Build time library dependencies are handled by the Go module automatically.

If you are on Go versions earlier than 1.11, you might need to run `go get -d
./...` to download all the dependencies. Note however, that modules always use
the same dependency versions, while `go get -d` always downloads master.

* Go 1.11.0 or later. Patches to support up to 2 prior major releases will
  be accepted if they are not overly intrusive and well written.
* See `go.mod`, `go.sum` and `go list -m -u all` for build time dependencies.

## References
[1] [Gong, Jiajun, and Tao Wang. "Zero-delay Lightweight Defenses against Website Fingerprinting." 29th USENIX Security Symposium. 2020.](https://www.usenix.org/system/files/sec20-gong.pdf)

[2] [Cai, Xiang, et al. "A Systematic Approach to Developing and Evaluating Website Fingerprinting Defenses." Proceedings of the 2014 ACM SIGSAC Conference on Computer and Communications Security. 2014.](https://dl.acm.org/doi/pdf/10.1145/2660267.2660362)

[3] [Wang, Tao, and Ian Goldberg. "Walkie-Talkie: An Efficient Defense Against Passive Website Fingerprinting Attacks." 26th USENIX Security Symposium. 2017.](https://www.usenix.org/system/files/conference/usenixsecurity17/sec17-wang-tao.pdf)

[4] [Yawning, Angel. "obfs4 - The obfourscator"](https://github.com/Yawning/obfs4)

[5] [Gong, Jiajun, et al. "WFDefProxy:Modularly Implementing and Empirically Evaluating Website Fingerprinting Defenses"](https://arxiv.org/abs/2111.12629)

## Disclaimer
This repository is only intended for research purpose. 
Codes may have bugs.
We do not guarantee it secure against any attacker in the real world. 
Please be cautious if you want to use it in the real Tor network.

[comment]: <> (## Thanks)

[comment]: <> ( * Yawning Angel for explaining the code of obfs4proxy)

[comment]: <> ( * Wuqi Zhang for providing the technical support for extending the framework.)