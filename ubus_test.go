package ubus

import (
	"log"
	"testing"

	"github.com/golangwrt/libubox"
)

type echoParam struct {
	Msg *string
}

type echoRes struct {
	Result string
}

var (
	ctx *Context
)

func init() {
	var err error
	ctx, err = Connect("")
	if err != nil {
		log.Panicf("connect to ubusd failed, %s\n", err)
	}
}

func ubusEcho(param *echoParam) *echoRes {
	var res echoRes
	if param != nil && param.Msg != nil {
		res.Result = *param.Msg
	} else {
		res.Result = "default"
	}
	return &res
}

func TestMethod(t *testing.T) {
	libubox.UloopInit()
	obj := NewObject("test")
	err := obj.RegisterMethod("echo", ubusEcho)
	if err != nil {
		t.Fatalf("register ubus method failed, %s\n", err)
	}
	if err = ctx.AddObject(obj); err != nil {
		t.Fatalf("add object failed, %s\n", err)
	}
	ctx.AddUloop()
	libubox.UloopRun()
}
