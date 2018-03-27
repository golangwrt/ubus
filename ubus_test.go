package ubus

import (
	"encoding/json"
	"fmt"
	"log"
	"runtime"
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

func TestServer(t *testing.T) {
	libubox.UloopInit()
	obj := NewObject("test")
	err := obj.RegisterMethod("echo", ubusEcho)
	if err != nil {
		t.Fatalf("register ubus method failed, %s\n", err)
	}
	if err = ctx.AddObject(obj); err != nil {
		t.Fatalf("add object failed, %s\n", err)
	}
	if err = ctx.AddUloop(); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("add ubus object test to uloop\n")

	go func() {
		runtime.Gosched()
		var id uint32
		var err error
		ctx.Run(func() {
			id, err = ctx.LookupID("test")
		}, DefaultInvokeTimeout)

		if err != nil {
			fmt.Printf("could not find ubus object test\n")
			t.Fail()
		} else {
			fmt.Printf("find test object: %d\n", id)
		}
	}()

	fmt.Printf("uloop run\n")
	libubox.UloopRun()
	fmt.Printf("uloop run end\n")
}

func TestCall(t *testing.T) {
	libubox.UloopInit()
	ctx, err := Connect(DefaultSockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = ctx.AddUloop(); err != nil {
		t.Fatal(err)
	}
	go func() {
		runtime.Gosched()
		var res json.RawMessage
		ctx.Run(func() {
			res, err = ctx.Call("test", "echo", json.RawMessage(`{"msg":"hello"}`), DefaultInvokeTimeout)
		}, DefaultInvokeTimeout)
		if err != nil {
			fmt.Printf("call failed, %s\n", err)
			t.Fail()
		} else {
			fmt.Printf("res is %s\n", string(res))
		}
		ctx.Run(func() { libubox.UloopEnd() }, 0)
	}()

	libubox.UloopRun()
}
