# WF EXPERIMENT SETTING

## Prerequisite

- System: Ubuntu (Centos is not working properly)
- Golang (recommended version: 1.16)
- Docker (0.19.0) Rootless docker is recommended, see [Rootless Manual](https://docs.docker.com/engine/security/rootless/) 
Update: Must use the rootless docker after I upgrade the Tor Browser to 12.0. TBB 12.0 somehow does not allow the root to run, and docker with root privilege can cause trouble.

## Components
 - **WFDefProxy** to implement WF defenses
 - **AlexaCrawler** as a Python script to automatically crawl a list
 - **dockersetup** as a packaged docker image to automate everything for the crawl (isolate the experiment environment as well)

## Get Prepared 
### 1. WFDefProxy
```
git clone git@github.com:websitefingerprinting/wfdef.git
cd wfdef/
git checkout dev-wfgan-origin
go build -o obfs4proxy/obfs4proxy ./obfs4proxy/
```
Then you can see the compiled binary in `./obfs4proxy/obfs4proxy`. 

### 2. AlexaCrawler
```
git clone git@github.com:websitefingerprinting/WFCrawler.git AlexaCrawler
cd AlexaCrawler/
git checkout dev-tbb-random
```
AlexaCrawler provides a full set of scripts to crawl and parse the raw traces. Branch `dev-tbb` is used together with `WFDefProxy`. 
You can replace the site list and other parameters in `AlexaCrawler/common.py`.
**Remember to rename the repository to `AlexaCrawler` to avoid path issues when using `dockersetup`, as introduced below. 
### 3. dockersetup
```
git clone git@github.com:websitefingerprinting/dockersetup.git
cd dockersetup
make build
```

### 4. dockersetup-server (Optinal)
```
git clone git@github.com:websitefingerprinting/dockersetup-server.git
cd dockersetup
make build
```
This is the docker image for setting up the bridge. Nearly the same as `dockersetup`, but the images is much more smaller, removing unnecessary packages. Following the usage of `dockersetup` if you want to use this repository to run a bridge inside a docker container. 

## Usage
### 1. First, make sure the bridge is running our defense 
You must have `WFDefProxy` built on the server too. An example torrc file for the bridge: 
```
DataDirectory /home/docker/tor-config/tunnel-proxy-hostport443
Log notice stdout
SOCKSPort auto
AssumeReachable 1
PublishServerDescriptor 0
Exitpolicy reject *:*
ORPort auto
ExtORPort auto
Nickname wfdefnull
BridgeRelay 1
ServerTransportPlugin null exec /home/wfdef/obfs4proxy/obfs4proxy
ServerTransportListenAddr null 0.0.0.0:35000
```
The defense we use is `null`, which is actually undefended. 

### 2. On the client side
```
cd dockersetup
vim Makefile
```
Double check the following:
- replace the `fingerprint` at line 53 with your bridge's identity key. It can be found at the launch like this: `
Your Tor server's identity key fingerprint is 'wfdefnull FC7EBA9228617F6C487B4C044DACFC203F6490C4'
`
- uncomment Line 59 and 60. It specifies the defense we use (i.e., `null`) and the parameters (`null` does not have the parameters). Otherwise, find the parameters on the server side (it's in `/home/docker/tor-config/tunnel-proxy-hostport443/pt_state/xxx_bridgeline.txt`). Check the examples from Line 58-79.
- Line 42, port number of the bridge.
- Line 45-46, the start and the end of the websites to crawl in the list
- Line 48-49, the crawler will crawl `m` x `b` instances for each website. (Since the round-robin fashion is disabled in branch `dev-tbb-random`, so only the production of these two parameters matters.)

Remember to replace the ip address of the bridge in `Entrypoint.sh` at Line 52.
`
echo 'Bridge '${wfd}' 40.121.250.145:'${port}' '${fingerprint}' '${cert}'' >> ${TORRC_PATH}
`

After everything is set correctly, you can start the crawl now by using
```
make run tag=test
```
`tag` provides a unique id for this crawler. You can check the logs in `~/tor-config/`. The crawled traces are saved in `~/AlexaCrawler/dump`. 

#####  Use of multiple crawlers
Since the crawling process is long, it is highly recommended to use multiple crawlers in parallel. You can use the script `screen_batch_mon.sh` to creat n crawling processes. Make sure to set the parameters correctly as mentioned in `Makefile` (Pay attention to Line 8-14).  
**Change the crawler number at Line 35. Then run 
```
bash screen_batch_mon.sh 
```
The crawl processes are running in separate screen sessions. 

## Post Processing
```
cd ~/AlexaCrawler
```

##### Merge the datasets
After the crawl, you can use `combine.py` to merge the datasets you crawled in parallel:
```
python3 combine.py -dir ./dump/ds1 ./dump/ds2 -d -o ./dump/
```
`-d` specifies NOT to delete the original folders and `-o` specifies the output dir. 

##### Parse raw traces
```
python3 parse_log.py ./dump/ds
```

The parsed datasets is in `./parsed/ds`. 

## Note 
You may come across an error in `AlexaCrawler/crawler.py` at the end of crawl. It is because of Line 338 which automatically sends me an email after the crawl is done. And you do not have that part of code in the repository. 

