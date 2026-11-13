module github.com/ZaneXo/zsmeet

go 1.26.0

// 前端依赖里有的包带着 Go 文件，别让 ./... 扫进去
ignore ./web/node_modules

require (
	github.com/alexedwards/scs/pgxstore v0.0.0-20251002162104-209de6e426de
	github.com/alexedwards/scs/v2 v2.9.0
	github.com/danielgtaylor/huma/v2 v2.40.0
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.12.0
	github.com/k2-fsa/sherpa-onnx-go v1.13.8
	github.com/larksuite/oapi-sdk-go/v3 v3.12.2
	github.com/openai/openai-go/v3 v3.97.0
	github.com/pressly/goose/v3 v3.29.0
	github.com/riverqueue/river v0.51.1
	github.com/riverqueue/river/riverdriver/riverpgxv5 v0.51.1
	golang.org/x/crypto v0.58.0
)

require (
	github.com/coder/websocket v1.8.15 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/k2-fsa/sherpa-onnx-go-linux v1.13.8 // indirect
	github.com/k2-fsa/sherpa-onnx-go-macos v1.13.8 // indirect
	github.com/k2-fsa/sherpa-onnx-go-windows v1.13.8 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/mozillazg/go-pinyin v0.21.0 // indirect
	github.com/riverqueue/river/riverdriver v0.51.1 // indirect
	github.com/riverqueue/river/rivershared v0.51.1 // indirect
	github.com/riverqueue/river/rivertype v0.51.1 // indirect
	github.com/sethvargo/go-retry v0.5.0 // indirect
	github.com/tidwall/gjson v1.20.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)
