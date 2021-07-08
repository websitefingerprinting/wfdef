package utils

import (
	"bufio"
	"fmt"
	pt "git.torproject.org/pluggable-transports/goptlib.git"
	"log"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

func SleepRho(lastSend time.Time, rho time.Duration)  {
	deltaT := time.Now().Sub(lastSend)
	if remainingDelay := rho - deltaT; remainingDelay > 0 {
		// We got data faster than the pacing rate, sleep
		// for the remaining time.
		time.Sleep(remainingDelay)
	}
}


func ParseArgByKey(args *pt.Args, key string, kind string) (interface{}, error) {
	kind = strings.ToLower(kind)
	Str, Ok := args.Get(key)
	if !Ok {
		return nil, fmt.Errorf("missing argument '%s'", key)
	}
	if kind == "int" {
		arg, err := strconv.Atoi(Str)
		if err != nil {
			return nil, fmt.Errorf("malformed '%s': '%s'", key, Str)
		}
		return arg, nil
	} else if kind == "float32" || kind == "float64" {
		precision, err := strconv.Atoi(kind[5:5+2])
		if err != nil {
			return nil, err
		}
		arg, err := strconv.ParseFloat(Str, precision)
		if err != nil {
			return nil, fmt.Errorf("malformed '%s': '%s'", key, Str)
		}
		return arg, nil
	}
	return nil, fmt.Errorf("wrong kind: '%s'", kind)
}


func ReadFloatFromFile(fdir string) []float64{
	file, err := os.Open(fdir)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	var arr []float64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lineStr := scanner.Text()
		num, sErr := strconv.ParseFloat(lineStr, 64)
		if sErr != nil {
			arr = append(arr,num)
		}
	}
	return arr
}


func SampleIPT(arr []float64) int{
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	std := arr[0]
	l := len(arr) -1
	ind := r.Intn(l) + 1
	res := arr[ind] + r.NormFloat64()*std
	res = math.Pow(10, res)
	res *= 1000  // seconds to milliseconds
	if res < 0 {
		//negative ipt
		res = 0
	} else if res > 500 {
		// ipt > 1s
		res = 500
	}
	return int(res)
}