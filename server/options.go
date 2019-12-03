package main

import (
	"regexp"
	"strconv"
	"time"
)

type Options struct {
	audioChNum     int
	subChNum       int
	grace          time.Duration
	duration       int
	forceTranscode bool
	preset         string
}

func OptionsFromString(in string) *Options {
	o := &Options{}
	re, _ := regexp.Compile(`a(\d+)`)
	res := re.FindStringSubmatch(in)
	if len(res) > 0 {
		i, err := strconv.Atoi(res[1])
		if err == nil {
			o.audioChNum = i
		}
	}
	re, _ = regexp.Compile(`s(\d+)`)
	res = re.FindStringSubmatch(in)
	if len(res) > 0 {
		i, err := strconv.Atoi(res[1])
		if err == nil {
			o.subChNum = i
		}
	}
	o.duration = 10
	o.forceTranscode = false
	return o
}
