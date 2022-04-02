module outline-go-tun2socks

go 1.15

require (
	github.com/Jigsaw-Code/choir v1.0.1
	github.com/Jigsaw-Code/getsni v0.0.0-20190807203514-efe2dbf35d1f
	github.com/ernestzhang/outline-go-tun2socks v0.0.0-20220402093652-95523d720b78
	github.com/ernestzhang/outline-ss-server v1.0.1
	github.com/eycorsican/go-tun2socks v1.16.11
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.1 // indirect
	github.com/songgao/water v0.0.0-20200317203138-2b4b6d7c09d8 // indirect
	golang.org/x/mobile v0.0.0-20200801112145-973feb4309de // indirect
	golang.org/x/net v0.0.0-20201006153459-a7d1128ccaa0
	golang.org/x/sys v0.0.0-20201007165808-a893ed343c85
)

replace github.com/Jigsaw-Code/outline-ss-server v1.3.5 => "../outline-ss-server"

replace github.com/ernestzhang/outline-ss-server => "../outline-ss-server"

replace github.com/ernestzhang/outline-go-tun2socks => "../outline-go-tun2socks"
