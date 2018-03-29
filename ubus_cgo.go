package ubus

/*
#cgo LDFLAGS: -lubox -lubus
#include <libubus.h>
#include <libubox/uloop.h>
extern int ubus_handler_stub(struct ubus_context *ctx, struct ubus_object *obj, struct ubus_request_data *req, const char *method, struct blob_attr *msg);
extern void ubus_data_handler_stub(struct ubus_request *req, int type, struct blob_attr *msg);
*/
import "C"
import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/golangwrt/libubox"
)

// Context encapsulates struct ubus_context
type Context struct {
	ptr *C.struct_ubus_context

	// Buf global BlobBuf corresponding with the ubus context,
	// could be reused safely in context driven by uloop.run,
	// e.g.: ubus method handler, uloop timer handler, uloop fd handler.
	Buf *libubox.BlobBuf

	sockPair [2]int
	ufdStub  *libubox.UloopFD
	ufdProxy *libubox.UloopFD
	calls    struct {
		Elem map[*call]chan error
		sync.RWMutex
	}
}

type DataHandler func(req *C.struct_ubus_request, _type C.int, msg *C.struct_blob_attr)

// Method encapsulates struct ubus_method
type Method struct {
	ptr    *C.struct_ubus_method
	Name   string
	f      reflect.Value
	p0Type reflect.Type
	r0Type reflect.Type
}

// Object encapsulates struct ubus_object
type Object struct {
	ptr     *C.struct_ubus_object
	Methods struct {
		Elem map[string]*Method
		sync.RWMutex
	}
	Name string
	Ctx  *Context
}

type call func()

const (
	DefaultSockPath      = "/var/run/ubus.sock"
	DefaultInvokeTimeout = time.Second * 3
)

var (
	objs struct {
		Elem map[*C.struct_ubus_object]*Object
		sync.RWMutex
	}
	requests struct {
		Elem map[*C.struct_ubus_request]DataHandler
		sync.RWMutex
	}
)

func init() {
	objs.Elem = make(map[*C.struct_ubus_object]*Object, 0)
	requests.Elem = make(map[*C.struct_ubus_request]DataHandler, 0)
}

// localMarshal using gob encoding the pointer of the object,
// the object is held in the ctx.calls map before call ends.
// it's safe to just send the pointer.
func (req *call) localMarshal() ([]byte, error) {
	buf := new(bytes.Buffer)
	enc := gob.NewEncoder(buf)
	err := enc.Encode(uintptr(unsafe.Pointer(req)))
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// localUnmarshalToCall get the call object from ctx.calls map
// using the gob decoded pointer value as the key
func localUnmarshalToCall(data []byte) (uintptr, error) {
	dec := gob.NewDecoder(bytes.NewReader(data))
	var p uintptr
	err := dec.Decode(&p)
	if err != nil {
		return uintptr(0), err
	}
	return p, err
}

//export ubusDataHandlerProxyGO
func ubusDataHandlerProxyGO(req *C.struct_ubus_request, _type C.int, msg *C.struct_blob_attr) {
	requests.Lock()
	defer requests.Unlock()
	if v, ok := requests.Elem[req]; ok {
		v(req, _type, msg)
		delete(requests.Elem, req)
	}
}

//export ubusHandlerProxyGO
func ubusHandlerProxyGO(ctx *C.struct_ubus_context, obj *C.struct_ubus_object, req *C.struct_ubus_request_data, method *C.char, msg *C.struct_blob_attr) C.int {
	mstr := C.GoString(method)

	// find Object instance
	objs.RLock()
	elem, found := objs.Elem[obj]
	objs.RUnlock()
	if !found {
		fmt.Fprintf(os.Stderr, "no Object found for object %p, method: %s\n", unsafe.Pointer(obj), mstr)
		return 0
	}

	var err error
	var param *libubox.JSONObject
	var in, out []reflect.Value

	elem.Methods.RLock()
	m, found := elem.Methods.Elem[mstr]
	elem.Methods.RUnlock()
	if !found {
		err = fmt.Errorf("no Method found for %s\n", mstr)
		goto sendReply
	}

	// call registered ubus callback function
	param, _ = libubox.NewJSONObjectWith(m.p0Type)
	err = param.UnmarshalBlobAttr(libubox.NewBlobAttrFromPointer(unsafe.Pointer(msg)))
	if err != nil {
		err = fmt.Errorf("unmarshal request parameter for ubus method %s failed: %s", mstr, err)
		goto sendReply
	}
	in = make([]reflect.Value, 1)
	in[0] = reflect.ValueOf(param.Value)
	out = m.f.Call(in)
sendReply:
	elem.Ctx.Buf.Init(0)
	if err != nil {
		elem.Ctx.Buf.AddString("error", err.Error())
	} else if len(out) > 0 && out[0].IsValid() {
		elem.Ctx.Buf.AddJsonFrom(out[0].Interface())
	}
	C.ubus_send_reply(ctx, req, (*C.struct_blob_attr)(elem.Ctx.Buf.Head().Pointer()))
	return 0
}

// RegisterMethod add a new ubus method with given name and function f
// f must have only one parameter and one result, both in type of "struct or struct pointer"
func (obj *Object) RegisterMethod(name string, f interface{}) error {
	v := reflect.ValueOf(f)
	if v.Kind() != reflect.Func {
		return fmt.Errorf("invalid type %s, require function", v.Kind().String())
	}

	// valid parameter and result of f
	t := v.Type()
	if t.NumIn() != 1 && t.NumOut() != 1 {
		return fmt.Errorf("invalid function with (%d in, %d out), require (1 in, 1 out)", t.NumOut(), t.NumOut())
	}
	var in reflect.Type
	in = t.In(0)
	if in.Kind() == reflect.Ptr {
		in = in.Elem()
	}
	if in.Kind() != reflect.Struct {
		return fmt.Errorf("invalid parameter type %s, require struct or struct pointer", in.Kind().String())
	}

	var out reflect.Type
	out = t.Out(0)
	if out.Kind() == reflect.Ptr {
		out = out.Elem()
	}
	if out.Kind() != reflect.Struct {
		return fmt.Errorf("invalid result type %s, require struct or struct pointer", out.Kind().String())
	}

	m := &Method{
		Name:   name,
		f:      v,
		p0Type: in,
		r0Type: out,
	}

	obj.Methods.Lock()
	tmp, _ := obj.Methods.Elem[m.Name]
	obj.Methods.Elem[m.Name] = m
	obj.Methods.Unlock()
	if tmp != nil && tmp.ptr != nil {
		C.free(unsafe.Pointer(m.ptr))
		m.ptr = nil
	}
	return nil
}

// Connect establish ubus connection using ubus_connect
//
// struct ubus_context *ubus_connect(const char *path)
//
// path: the unix domain socket path for ubus daemon
// pass empty string will use the DefaultSockPath "/var/run/ubus.sock"
//
func Connect(path string) (context *Context, err error) {
	var cpath *C.char
	if path != "" {
		cpath = C.CString(path)
		defer C.free(unsafe.Pointer(cpath))
	}
	var ctx *C.struct_ubus_context
	ctx, err = C.ubus_connect(cpath)
	if err != nil {
		return nil, err
	}
	return &Context{
		ptr: ctx,
		Buf: libubox.NewBlobBuf(),
	}, nil
}

// NewObject create an ubus object
func NewObject(name string) *Object {
	cname := C.CString(name)

	objType := (*C.struct_ubus_object_type)(C.calloc(1, C.sizeof_struct_ubus_object_type))
	objType.name = cname
	objType.id = 0
	objType.n_methods = 0

	p := (*C.struct_ubus_object)(C.calloc(1, C.sizeof_struct_ubus_object))
	p._type = (*C.struct_ubus_object_type)(objType)
	p.name = objType.name
	p.n_methods = objType.n_methods

	obj := &Object{
		Name: name,
		ptr:  p,
	}
	obj.Methods.Elem = make(map[string]*Method)
	return obj
}

func (ctx *Context) ufdStubHandler(ufd *libubox.UloopFD, events uint) {
	elem := ufd.Read()
	defer elem.Free()

	if elem.Err != nil {
		fmt.Fprintf(os.Stderr, "ufdStubHandler: Read failed: %s", elem.Err)
		return
	}
	var err error
	var p uintptr

	if p, err = localUnmarshalToCall(elem.Data); err != nil {
		fmt.Fprintf(os.Stderr, "ufdStubHandler: decode response failed: %s", err)
		return
	}

	c := (*call)(unsafe.Pointer(p))
	ctx.calls.RLock()
	done, found := ctx.calls.Elem[c]
	ctx.calls.RUnlock()

	if !found {
		fmt.Fprintf(os.Stderr, "ufdStubHandler: %p is not in ongoning calls", c)
		return
	}
	if done != nil {
		done <- nil
	}
}

func (ctx *Context) ufdProxyHandler(ufd *libubox.UloopFD, events uint) {
	elem := ufd.Read()
	defer elem.Free()

	if elem.Err != nil {
		fmt.Fprintf(os.Stderr, "ufdProxyHandler: Read failed: %s", elem.Err)
		return
	}
	var p uintptr
	var err error
	if p, err = localUnmarshalToCall(elem.Data); err != nil {
		fmt.Fprintf(os.Stderr, "ufdProxyHandler: decode request failed: %s", err)
		return
	}

	req := (*call)(unsafe.Pointer(p))
	if req != nil {
		(*req)()
	}

	ctx.calls.RLock()
	_, found := ctx.calls.Elem[req]
	ctx.calls.RUnlock()
	if !found {
		return
	}
	_, err = ctx.ufdProxy.Write(elem.Data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ufdProxyHandler: write response failed, %s", err)
		return
	}
}

// AddUloop add the fd of ubus context to uloop
func (ctx *Context) AddUloop() error {
	ret, err := C.uloop_fd_add(&ctx.ptr.sock, C.ULOOP_BLOCKING|C.ULOOP_READ)
	if err != nil {
		return fmt.Errorf("ret: %d, %s", ret, err)
	}
	ctx.calls.Elem = make(map[*call]chan error, 0)
	ctx.sockPair, err = syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	ctx.ufdStub = libubox.NewUloopFD(ctx.sockPair[0], ctx.ufdStubHandler)
	ctx.ufdProxy = libubox.NewUloopFD(ctx.sockPair[1], ctx.ufdProxyHandler)

	err = syscall.SetsockoptInt(ctx.sockPair[0], syscall.SOL_SOCKET, syscall.SO_SNDBUF, libubox.UFDReadBufferSize)
	err = syscall.SetsockoptInt(ctx.sockPair[0], syscall.SOL_SOCKET, syscall.SO_RCVBUF, libubox.UFDReadBufferSize)
	err = syscall.SetsockoptInt(ctx.sockPair[1], syscall.SOL_SOCKET, syscall.SO_SNDBUF, libubox.UFDReadBufferSize)
	err = syscall.SetsockoptInt(ctx.sockPair[1], syscall.SOL_SOCKET, syscall.SO_RCVBUF, libubox.UFDReadBufferSize)

	ctx.ufdStub.Add(libubox.UloopBlocking | libubox.UloopRead)
	ctx.ufdProxy.Add(libubox.UloopBlocking | libubox.UloopRead)
	return nil
}

// RemoveObject remove the object from the ubus connection
//
// int ubus_remove_object(struct ubus_context *ctx, struct ubus_object *obj)
func (ctx *Context) RemoveObject(obj *Object) error {
	if obj == nil {
		return fmt.Errorf("%s", "nil parameter: obj")
	}
	ret := C.ubus_remove_object(ctx.ptr, obj.ptr)
	if ret != C.UBUS_STATUS_OK {
		return fmt.Errorf("%s", ubusErrorString(ret))
	}
	return nil
}

// AddObject make an object visible to remote connections
//
// int ubus_add_object(struct ubus_context *ctx, struct ubus_object *obj)
func (ctx *Context) AddObject(obj *Object) error {
	if obj == nil {
		return fmt.Errorf("%s", "nil obj")
	}
	if obj.ptr == nil {
		return fmt.Errorf("%s", "nil obj.ptr")
	}
	num := len(obj.Methods.Elem)
	if num == 0 {
		return fmt.Errorf("%s", "no ubus method registered with obj")
	}

	buf := C.calloc(1, C.size_t(C.sizeof_struct_ubus_method*num))
	cmethods := (*[1 << 16]C.struct_ubus_method)(unsafe.Pointer(buf))
	var i int
	for _, v := range obj.Methods.Elem {
		cmethods[i].name = C.CString(v.Name)
		cmethods[i].tags = 0
		cmethods[i].handler = (C.ubus_handler_t)(C.ubus_handler_stub)
		i++
	}
	obj.ptr._type.methods = (*C.struct_ubus_method)(unsafe.Pointer(buf))
	obj.ptr._type.n_methods = C.int(num)

	obj.ptr.methods = obj.ptr._type.methods
	obj.ptr.n_methods = obj.ptr._type.n_methods

	ret := C.ubus_add_object(ctx.ptr, obj.ptr)
	if ret != C.UBUS_STATUS_OK {
		return fmt.Errorf("ubus_add_object: %s", ubusErrorString(ret))
	}
	objs.Lock()
	objs.Elem[obj.ptr] = obj
	objs.Unlock()
	obj.Ctx = ctx
	return nil
}

// Run: schedule the function to be run in context of uloop.run
// timeout: maximum duration to wait the call finished,
//     * 0: non wait (async call)
//     * negative: wait infinitely
//     * positive: wait specified duration
func (ctx *Context) Run(f call, timeout time.Duration) error {
	req := &f

	var err error
	var data []byte

	if data, err = req.localMarshal(); err != nil {
		return err
	}

	ctx.calls.Lock()
	ctx.calls.Elem[req] = make(chan error)
	ctx.calls.Unlock()

	defer func() {
		ctx.calls.Lock()
		close(ctx.calls.Elem[req])
		delete(ctx.calls.Elem, req)
		ctx.calls.Unlock()
	}()

	var n int
	if n, err = ctx.ufdStub.Write(data); err != nil {
		return fmt.Errorf("write to stub fd failed, n: %d, err: %s", n, err)
	}

	if timeout == 0 {
		return nil
	}

	if timeout < 0 {
		err = <-ctx.calls.Elem[req]
	} else {
		t := time.NewTimer(timeout)
		select {
		case err = <-ctx.calls.Elem[req]:
			break
		case <-t.C:
			err = fmt.Errorf("req: %+v timeout after %s", req, timeout)
		}
		if !t.Stop() {
			<-t.C
		}
	}
	return err
}

// Call does the same as CLI 'ubus -t timeout call path method param'
//
// PS. it must be called in the context of uloop.run, if you are not sure,
// wrap your invocation with Context.Run
func (ctx *Context) Call(path, method string, param json.RawMessage, timeout time.Duration) (resp json.RawMessage, err error) {
	var id uint32
	if id, err = ctx.LookupID(path); err != nil {
		return nil, err
	}
	return ctx.Invoke(id, method, param, int(timeout/time.Millisecond))
}

// LookupID encapsulates ubus_lookup_id
//
// int ubus_lookup_id(struct ubus_context *ctx, const char *path, uint32_t *id)
//
// PS. it must be called in the context of uloop.run, if you are not sure,
// wrap your invocation with Context.Run
func (ctx *Context) LookupID(path string) (uint32, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))

	var id C.uint32_t
	ret, err := C.ubus_lookup_id(ctx.ptr, cpath, &id)
	if ret != C.UBUS_STATUS_OK {
		return 0, fmt.Errorf("ret: %d, %s, %s", int(ret), err, ubusErrorString(ret))
	}
	return uint32(id), nil
}

// Invoke encapsulates ubus_invoke, it's better to use Call instead of this one.
//
// int ubus_invoke(struct ubus_context *ctx, uint32_t obj, const char *method, struct blob_attr *msg, ubus_data_handler_t cb, void *priv, int timeout)
//
// PS. this method must be called in the same context of uloop.run
// in fact, it does not call ubus_invoke directly, instead, it re-implements ubus_invoke_fd,
// as we need create the mapping from the ubus_request pointer to the DataHandler implemented in go
func (ctx *Context) Invoke(id uint32, method string, param json.RawMessage, timeoutMS int) (response json.RawMessage, err error) {
	cmethod := C.CString(method)
	defer C.free(unsafe.Pointer(cmethod))

	// validate request param
	ctx.Buf.Init(0)
	if param != nil {
		if err = ctx.Buf.AddJsonFromString(string(param)); err != nil {
			return nil, fmt.Errorf("invalid param: %s", err)
		}
	}

	// allocate struct ubus_request
	req := (*C.struct_ubus_request)(C.calloc(1, C.sizeof_struct_ubus_request))
	defer C.free(unsafe.Pointer(req))

	var ret C.int
	if ret, err = C.ubus_invoke_async_fd(ctx.ptr, C.uint32_t(id), cmethod, (*C.struct_blob_attr)(ctx.Buf.Head().Pointer()), req, -1); ret != C.UBUS_STATUS_OK {
		return nil, fmt.Errorf("ret: %d, %s, %s", int(ret), err, ubusErrorString(ret))
	}

	cb := func(req *C.struct_ubus_request, _type C.int, msg *C.struct_blob_attr) {
		response = json.RawMessage(libubox.NewBlobAttrFromPointer(unsafe.Pointer(msg)).FormatJSON(true))
	}

	req.data_cb = C.ubus_data_handler_t(C.ubus_data_handler_stub)
	req.priv = nil

	requests.Lock()
	requests.Elem[req] = cb
	requests.Unlock()

	if ret, err = C.ubus_complete_request(ctx.ptr, req, C.int(timeoutMS)); ret != C.UBUS_STATUS_OK {
		requests.Lock()
		delete(requests.Elem, req)
		requests.Unlock()
		return nil, fmt.Errorf("ret: %d, %s, %s", int(ret), err, ubusErrorString(ret))
	}

	return response, nil
}

func ubusErrorString(ret C.int) string {
	return C.GoString(C.ubus_strerror(ret))
}
