# ubus
libubus wrapper

## prerequsites
- libubox: https://git.openwrt.org/project/libubox.git
- ubus: https://git.openwrt.org/project/ubus.git

## build

1. libubus and libubox could be found by default flags

    `go build`

    CGO_ENABLED is set by default for most goarch and goos, in that case,
    you can check the default configuration by **go env**, if **CGO_ENABLED**
    is not set, you need build with:

    `CGO_ENABLED=1 go build`

2. custom location for libubus and libubox

    `CGO_ENABLED=1 CGO_CFLAGS=-I/path/to/header/dir CGO_LDFLAGS=-L/path/to/lib/dir go build`

3. if you need cross compile for other architecture:

    `CC=/path/to/compiler ...`

    e.g.: to cross compile for openwrt running on MIPS:

    ```
    CC=mips-openwrt-linux-gcc CGO_ENABLED=1 \
    CGO_LDFLAGS="-L/opt/packages/lede_repo/lede/staging_dir/target-mips_musl/usr/lib" \
    CGO_CFLAGS="-I/opt/packages/lede_repo/lede/staging_dir/target-mips_musl/usr/include"\
    GOARCH=mips go build
    ```

