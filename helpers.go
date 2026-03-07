package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

var aliasAdjectives = []string{"Swift", "Bold", "Clever", "Bright", "Quick", "Sharp", "Wise", "Calm", "Brave", "Cool"}
var aliasNouns = []string{"Fox", "Eagle", "Wolf", "Hawk", "Lion", "Tiger", "Bear", "Deer", "Bird", "Fish"}

func generateAlias() string {
	adj := aliasAdjectives[randInt(len(aliasAdjectives))]
	noun := aliasNouns[randInt(len(aliasNouns))]
	num := randInt(999) + 1
	return fmt.Sprintf("%s%s%d", adj, noun, num)
}

func randInt(max int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}

func sanitiseField(s string) string {
	s = strings.ReplaceAll(s, "|", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}
