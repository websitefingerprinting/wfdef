package regulator

import (
	"github.com/websitefingerprinting/wfdef.git/common/utils"
	"math"
	"sync"
	"time"
)

type serverParams struct {
	lastSend      time.Time //record the start of a session (!not a packet)
	targetRate    int32
	paddingBudget int32
	mutex         sync.RWMutex
}

func (sp *serverParams) SetLastSend() {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()
	sp.lastSend = time.Now()
}

func (sp *serverParams) GetLastSend() time.Time {
	sp.mutex.RLock()
	defer sp.mutex.RUnlock()
	return sp.lastSend
}

func (sp *serverParams) CalTargetRate(currentTime time.Time, r int32, d float32) (newRate int32, isChanged bool) {
	lastSend := sp.GetLastSend()
	timeElapsed := currentTime.Sub(lastSend)
	//log.Debugf("[DEBUG] Current time %v, last time %v, elapsed time: %v", currentTime.Format("15:04:05.000000"), lastSend.Format("15:04:05.000000"), timeElapsed)
	if timeElapsed > 0 {
		newRate = int32(float64(r) * math.Pow(float64(d), timeElapsed.Seconds()))
		if newRate < 1 {
			newRate = 1
		}
	}
	isChanged = false
	if sp.GetTargetRate() != newRate {
		sp.SetTargetRate(newRate)
		isChanged = true
	}
	return
}

func (sp *serverParams) SetTargetRate(targetRate int32) {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()
	sp.targetRate = targetRate
}

func (sp *serverParams) GetTargetRate() int32 {
	sp.mutex.RLock()
	defer sp.mutex.RUnlock()
	return sp.targetRate
}

func (sp *serverParams) SetPaddingBudget(n int32) {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()
	sp.paddingBudget = int32(utils.Uniform(0, int(n)))
}

func (sp *serverParams) GetPaddingBudget() int32 {
	sp.mutex.RLock()
	defer sp.mutex.RUnlock()
	return sp.paddingBudget
}

func (sp *serverParams) ConsumePaddingBudget() bool {
	sp.mutex.Lock()
	defer sp.mutex.Unlock()
	if sp.paddingBudget > 0 {
		sp.paddingBudget -= 1
		return true
	} else {
		return false
	}
}

func (sp *serverParams) Init(r int32, n int32) {
	// just to give an initial value for each param
	sp.SetTargetRate(r)
	sp.SetPaddingBudget(n)
	sp.SetLastSend()
}
