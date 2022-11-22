package wfgan

const (
	stateStart = iota
	stateStop
	stateReady
)

var stateMap = map[uint32]string {
	stateStart: "stateStart",
	stateStop: "stateStop",
	stateReady: "stateReady",
}