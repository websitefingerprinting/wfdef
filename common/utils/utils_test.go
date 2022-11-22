package utils

import (
	"fmt"
	"math"
	"testing"
)

func TestReadFloatFromFile(t *testing.T) {
	arr :=ReadFloatFromFile("../../transports/wfgan/grpc/time_feature_0-100x0-1000_o2o.ipt")
	if math.Abs(arr[0] - 0.002031) >= 1e-6 {
		t.Fatal("error")
	}
	if len(arr) != 10000000+1 {
		t.Fatal("Error")
	}
}

func TestRandomBernoulli(t *testing.T) {
	p := 0.4
	arr := [1000]int{}
	sum := 0.0
	for i:=0; i<len(arr); i++ {
		arr[i] = RandomBernoulli(p)
		sum += float64(arr[i]) / float64(len(arr))
	}
	fmt.Printf("%.2f\n", sum)
}

