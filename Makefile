.PHONY: test build vet proto test-e2e cluster-start cluster-stop cluster-verify pd-cluster-start pd-cluster-stop pd-cluster-verify

CLUSTER_DIR = /tmp/gookv-cluster
CLUSTER_NODES = 5
CLUSTER_TOPOLOGY = 1=127.0.0.1:20160,2=127.0.0.1:20161,3=127.0.0.1:20162,4=127.0.0.1:20163,5=127.0.0.1:20164

PD_CLUSTER_DIR = /tmp/gookv-pd-cluster
PD_ADDR = 127.0.0.1:2379

test:
	go test ./pkg/... ./internal/... -v -count=1

test-e2e:
	go test ./e2e/... -v -count=1 -timeout 120s

build:
	go build -o gookv-server ./cmd/gookv-server
	go build -o gookv-ctl ./cmd/gookv-ctl
	go build -o gookv-pd ./cmd/gookv-pd

vet:
	go vet ./...

proto:
	@echo "Proto generation is not needed: gookv uses pre-generated Go code from github.com/pingcap/kvproto"

pd-cluster-start: build
	@echo "Starting PD + $(CLUSTER_NODES)-node gookv cluster..."
	@mkdir -p $(PD_CLUSTER_DIR)/pd
	@./gookv-pd \
		--addr $(PD_ADDR) \
		--cluster-id 1 \
		--data-dir $(PD_CLUSTER_DIR)/pd \
		> $(PD_CLUSTER_DIR)/pd.log 2>&1 & \
	echo $$! > $(PD_CLUSTER_DIR)/pd.pid; \
	echo "  PD: addr=$(PD_ADDR) pid=$$(cat $(PD_CLUSTER_DIR)/pd.pid)"
	@sleep 1
	@for i in $$(seq 1 $(CLUSTER_NODES)); do \
		GRPC_PORT=$$((20159 + $$i)); \
		STATUS_PORT=$$((20179 + $$i)); \
		DATA_DIR=$(PD_CLUSTER_DIR)/node$$i; \
		PID_FILE=$(PD_CLUSTER_DIR)/node$$i.pid; \
		LOG_FILE=$(PD_CLUSTER_DIR)/node$$i.log; \
		mkdir -p $$DATA_DIR; \
		./gookv-server \
			--store-id $$i \
			--addr 127.0.0.1:$$GRPC_PORT \
			--status-addr 127.0.0.1:$$STATUS_PORT \
			--data-dir $$DATA_DIR \
			--pd-endpoints $(PD_ADDR) \
			--initial-cluster $(CLUSTER_TOPOLOGY) \
			> $$LOG_FILE 2>&1 & \
		echo $$! > $$PID_FILE; \
		echo "  Node $$i: gRPC=127.0.0.1:$$GRPC_PORT status=127.0.0.1:$$STATUS_PORT pd=$(PD_ADDR) pid=$$(cat $$PID_FILE)"; \
	done
	@echo "PD cluster started. Use 'make pd-cluster-stop' to shut down."

pd-cluster-stop:
	@echo "Stopping PD cluster..."
	@for i in $$(seq 1 $(CLUSTER_NODES)); do \
		PID_FILE=$(PD_CLUSTER_DIR)/node$$i.pid; \
		if [ -f $$PID_FILE ]; then \
			PID=$$(cat $$PID_FILE); \
			if kill -0 $$PID 2>/dev/null; then \
				kill $$PID; \
				echo "  Node $$i (pid $$PID): stopped"; \
			else \
				echo "  Node $$i (pid $$PID): already stopped"; \
			fi; \
			rm -f $$PID_FILE; \
		fi; \
	done
	@PID_FILE=$(PD_CLUSTER_DIR)/pd.pid; \
	if [ -f $$PID_FILE ]; then \
		PID=$$(cat $$PID_FILE); \
		if kill -0 $$PID 2>/dev/null; then \
			kill $$PID; \
			echo "  PD (pid $$PID): stopped"; \
		else \
			echo "  PD (pid $$PID): already stopped"; \
		fi; \
		rm -f $$PID_FILE; \
	fi
	@rm -rf $(PD_CLUSTER_DIR)
	@echo "PD cluster stopped and data cleaned up."

pd-cluster-verify:
	@echo "Verifying PD cluster replication..."
	@go run scripts/pd-cluster-verify/main.go
