module github.com/NVIDIA/nke/controller/forge-vms

go 1.24.4

toolchain go1.24.5

require (
	github.com/google/uuid v1.6.0
	github.com/lmittmann/tint v1.1.3
	github.com/osrg/gobgp/v3 v3.37.0
	github.com/uptrace/bun v1.2.11
	github.com/uptrace/bun/dialect/sqlitedialect v1.2.11
	github.com/vishvananda/netlink v1.3.1
	gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go v0.0.9
	go.yaml.in/yaml/v4 v4.0.0-rc.3
	golang.org/x/crypto v0.48.0
	modernc.org/sqlite v1.40.1
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	golang.org/x/exp v0.0.0-20251113190631-e25ba8c21ef6 // indirect
	golang.org/x/sys v0.41.0 // indirect
	modernc.org/libc v1.67.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace gitlab-master.nvidia.com/nvmetal/cloud-api-sdk-go => gitlab-master.nvidia.com/nvmetal/carbide-rest-ngc-sdk-go v0.0.9
