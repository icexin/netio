# Overview

A ssh like remote command execution tool without annoying shell escape

# Install

`go get github.com/icexin/netio`

# Usage

server:

`netio -s -addr=:9000`

client:

`netio -addr=:127.0.0.1:9000 -t top`

More usage see `netio -h`

# Profile

``` bash
$ cat ~/.netiorc
addr="127.0.0.1:9000"
```

Then you can use netio client without `-addr` flag.


