module github.com/dsx-ai-factory/nvke/controller

go 1.27.3

require (
	github.com/GehirnInc/crypt v0.0.0-20230320061759-8cc1b52080c5
	github.com/blang/semver/v4 v4.0.0
	github.com/caarlos0/env/v11 v11.4.1
	github.com/dsx-ai-factory/cluster-api-provider-nico v0.1.0-rc.2.0.20260929174319-72784d3eaafc
	github.com/dsx-ai-factory/nvke/agent v0.0.0-00010101000000-000000000000
	github.com/dsx-ai-factory/nvke/auth v0.0.0-00010101000000-000000000000
	github.com/dsx-ai-factory/nvke/controller/api v0.0.0-00010101000000-000000000000
	github.com/dsx-ai-factory/nvke/management-engine v0.0.0-00010101000000-000000000000
	github.com/evanphx/json-patch/v5 v5.9.11
	github.com/getkin/kin-openapi v0.151.0
	github.com/go-jose/go-jose/v4 v4.1.6
	github.com/go-logr/logr v1.4.4
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/cel-go v0.29.2
	github.com/google/uuid v1.6.0
	github.com/gravitational/teleport/api v0.0.0-20251224210154-05b9362bd4d6
	github.com/gravitational/trace v1.5.4
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/oapi-codegen/nethttp-middleware v1.2.0
	github.com/oapi-codegen/runtime v1.7.0
	github.com/prometheus/client_golang v1.25.0
	github.com/prometheus/client_model v0.6.3
	github.com/spiffe/go-spiffe/v2 v2.9.0
	github.com/spiffe/spire-api-sdk v1.15.3
	github.com/spiffe/spire-controller-manager v0.8.0
	github.com/stretchr/testify v1.12.1
	github.com/swaggest/swgui v1.8.9
	github.com/uber-go/tally/v4 v4.1.17
	github.com/uptrace/bun v1.3.0
	github.com/uptrace/bun/dialect/pgdialect v1.3.0
	github.com/uptrace/bun/dialect/sqlitedialect v1.3.0
	github.com/uptrace/bun/driver/pgdriver v1.3.0
	github.com/uptrace/bun/extra/bunotel v1.3.0
	github.com/uptrace/bun/extra/bunslog v1.3.0
	go.etcd.io/gofail v0.2.0
	go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.72.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.72.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.47.0
	go.opentelemetry.io/otel/metric v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	go.temporal.io/api v1.64.0
	go.temporal.io/sdk v1.50.0
	go.temporal.io/sdk/contrib/opentelemetry v0.8.1
	go.temporal.io/sdk/contrib/tally v0.2.0
	go.uber.org/automaxprocs v1.6.0
	go.yaml.in/yaml/v4 v4.0.0-rc.6
	golang.org/x/sync v0.24.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/api v0.37.1
	k8s.io/apiextensions-apiserver v0.37.1
	k8s.io/apimachinery v0.37.1
	k8s.io/apiserver v0.37.1
	k8s.io/client-go v0.37.1
	k8s.io/component-base v0.37.1
	k8s.io/kube-proxy v0.37.1
	k8s.io/kubelet v0.37.1
	k8s.io/utils v0.0.0-20260626114624-be93311217bd
	sigs.k8s.io/cluster-api v1.13.7
	sigs.k8s.io/controller-runtime v0.25.2
	sigs.k8s.io/json v0.0.0-20250730193827-2d320260d730
	sigs.k8s.io/yaml v1.6.0
)

require (
	cel.dev/expr v0.25.2 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/beevik/etree v1.6.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/charlievieth/strcase v0.0.6 // indirect
	github.com/coreos/go-oidc v2.5.0+incompatible // indirect
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/emicklei/go-restful/v3 v3.13.0 // indirect
	github.com/facebookgo/clock v0.0.0-20150410010913-600d898af40a // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/fxamacker/cbor/v2 v2.9.1 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-openapi/jsonpointer v1.0.1 // indirect
	github.com/go-openapi/jsonreference v1.0.2 // indirect
	github.com/go-openapi/swag v0.29.2 // indirect
	github.com/go-openapi/swag/cmdutils v0.29.2 // indirect
	github.com/go-openapi/swag/conv v0.29.2 // indirect
	github.com/go-openapi/swag/fileutils v0.29.2 // indirect
	github.com/go-openapi/swag/jsonutils v0.29.2 // indirect
	github.com/go-openapi/swag/loading v0.29.2 // indirect
	github.com/go-openapi/swag/mangling v0.29.2 // indirect
	github.com/go-openapi/swag/netutils v0.29.2 // indirect
	github.com/go-openapi/swag/pools v0.29.2 // indirect
	github.com/go-openapi/swag/stringutils v0.29.2 // indirect
	github.com/go-openapi/swag/typeutils v0.29.2 // indirect
	github.com/go-openapi/swag/yamlutils v0.29.2 // indirect
	github.com/go-piv/piv-go/v2 v2.3.0 // indirect
	github.com/gobuffalo/flect v1.0.3 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.4.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/mock v1.7.0-rc.1 // indirect
	github.com/google/gnostic-models v0.7.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/grpc-ecosystem/go-grpc-middleware/v2 v2.3.3 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.32.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mattermost/xml-roundtrip-validator v0.1.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nexus-rpc/nexus-proto-annotations v0.1.1-0.20260629224650-76486e7cc189 // indirect
	github.com/nexus-rpc/sdk-go v0.7.0 // indirect
	github.com/oasdiff/yaml v0.1.1 // indirect
	github.com/oasdiff/yaml3 v0.0.14 // indirect
	github.com/onsi/gomega v1.44.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/pquerna/cachecontrol v0.1.0 // indirect
	github.com/prometheus/common v0.72.1 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/robfig/cron v1.2.0 // indirect
	github.com/russellhaering/gosaml2 v0.11.0 // indirect
	github.com/russellhaering/goxmldsig v1.6.0 // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/twmb/murmur3 v1.1.8 // indirect
	github.com/uptrace/opentelemetry-go-extra/otelsql v0.3.2 // indirect
	github.com/vearutop/statigz v1.4.0 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/exp v0.0.0-20261014143641-f0251eb4aa3e // indirect
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/term v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	gomodules.xyz/jsonpatch/v2 v2.5.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261012220534-4341edf3f941 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261005182115-fad411399dd8 // indirect
	gopkg.in/evanphx/json-patch.v4 v4.13.0 // indirect
	gopkg.in/go-jose/go-jose.v2 v2.6.3 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	k8s.io/cluster-bootstrap v0.35.4 // indirect
	k8s.io/klog/v2 v2.140.0 // indirect
	k8s.io/kube-openapi v0.0.0-20260721132016-d427ff9ee9ad // indirect
	mellium.im/sasl v0.3.2 // indirect
	sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.36.0 // indirect
	sigs.k8s.io/randfill v1.0.0 // indirect
	sigs.k8s.io/structured-merge-diff/v6 v6.4.2 // indirect
)

replace (
	github.com/dsx-ai-factory/nvke/agent => ../agent
	github.com/dsx-ai-factory/nvke/auth => ../auth
	github.com/dsx-ai-factory/nvke/controller/api => ./api
	github.com/dsx-ai-factory/nvke/management-engine => ../management-engine
)

// go.temporal.io/sdk/contrib/tally (latest release) still requires an ancient
// temporal sdk whose graph mentions the pre-split google.golang.org/genproto,
// which collides with the genproto/googleapis/* split modules in the workspace
// build list ("ambiguous import"). Pin the stub monolithic module, which no
// longer carries the duplicated packages.
replace google.golang.org/genproto => google.golang.org/genproto v0.0.0-20260810153831-ec0a7760b754
