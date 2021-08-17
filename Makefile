.PHONY: build
build:
	go build -o dist/promscale ./cmd/promscale

test:
	go test -v -race ./...

e2e-test:
	go test -v ./pkg/tests/end_to_end_tests/ -use-extension=false
	go test -v ./pkg/tests/end_to_end_tests/ -use-extension=false
	go test -v ./pkg/tests/end_to_end_tests/ -use-extension=false -use-timescaledb=false
	go test -v ./pkg/tests/end_to_end_tests/ -use-timescale2
	go test -v ./pkg/tests/end_to_end_tests/ -use-extension=false -use-timescale2
	go test -v ./pkg/tests/end_to_end_tests/ -use-multinode


go-fmt:
	gofmt -d .

go-lint:
	golangci-lint run --timeout=5m --skip-dirs=pkg/promql --skip-dirs=pkg/promb

all: build test e2e-test go-fmt go-lint


promscale-install:
	-kubectl create namespace timescaledb
	-kubectl config set-context --current --namespace=timescaledb
	-kubectl get all
	-curl --proto '=https' --tlsv1.2 -sSLf  https://tsdb.co/install-tobs-sh |sh
	-tobs --namespace timescaledb install
	-kubectl get all -n timescaledb
	-tobs --namespace timescaledb grafana get-password
	-echo "tobs --namespace timescaledb grafana port-forward - to port forward to grafana"
	-echo "Port forward to postgres"
	-echo "kubectl -n timescaledb port-forward pod/tobs-timescaledb-0 5432:5432"
	-echo ""

promscale-clean:
	-kubectl delete namespace timescaledb
	-sleep 10
	-kubectl get all -n timescaledb