package ubus

/*
#cgo LDFLAGS: -lubox -lubus
#include <libubus.h>
extern int ubus_handler_stub(struct ubus_context *ctx, struct ubus_object *obj, struct ubus_request_data *req, const char *method, struct blob_attr *msg);
*/
import "C"
import (
	"fmt"
	"os"
	"reflect"
	"sync"
	"unsafe"

	"github.com/golangwrt/libubox"
)

// Context encapsulates struct ubus_context
type Context struct {
	ptr  *C.struct_ubus_context
	Buf  *libubox.BlobBuf
}


type Handler func(param *libubox.JSONObject) *libubox.JSONObject

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
	ptr *C.struct_ubus_object
	Methods struct {
		Elem map[string]*Method
		sync.RWMutex
	}
	Name string
	Ctx *Context
}

const (
	DefaultUbusdPath = "/var/run/ubus.sock"
)

var (
	objs struct {
		Elem map[*C.struct_ubus_object]*Object
		sync.RWMutex
	}
)

func init() {
	objs.Elem = make(map[*C.struct_ubus_object]*Object, 0)
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
func (obj *Object) RegisterMethod(name string, f interface {}) error {
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
// struct ubus_context *ubus_connect(const char *path)
//
// path: the unix domain socket path for ubus daemon
// pass empty string will use the DefaultUbusdPath "/var/run/ubus.sock"
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
		ptr: p,
	}
	obj.Methods.Elem = make(map[string]*Method)
	return obj
}

func (ctx *Context) AddUloop() error {
	_, err := C.ubus_add_uloop(ctx.ptr)
	return err
}

// RemoveObject remove the object from the ubus connection
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
		return fmt.Errorf("%s", ubusErrorString(ret))
	}
	objs.Lock()
	objs.Elem[obj.ptr] = obj
	objs.Unlock()
	obj.Ctx = ctx
	return nil
}

func ubusErrorString(ret C.int) string {
	return C.GoString(C.ubus_strerror(ret))
}
