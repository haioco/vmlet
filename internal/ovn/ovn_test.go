package ovn

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"testing"
	"time"

	"github.com/go-logr/stdr"
	"github.com/ovn-org/libovsdb/database/inmemory"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/libovsdb/server"
)

func TestMain(m *testing.M) {
	// The libovsdb test server is extremely chatty at the default verbosity.
	stdr.SetVerbosity(0)
	os.Exit(m.Run())
}

// startTestNB starts an in-memory OVSDB server that speaks the OVN_Northbound
// subset of the schema and returns a client connected to it.
func startTestNB(t *testing.T) (*Client, func()) {
	t.Helper()

	schemaJSON, err := os.ReadFile("testdata/ovn-nb.min.ovsschema")
	if err != nil {
		t.Fatalf("reading test schema: %v", err)
	}
	var schema ovsdb.DatabaseSchema
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		t.Fatalf("parsing test schema: %v", err)
	}

	dbModel, err := NewClientDBModel()
	if err != nil {
		t.Fatalf("building client db model: %v", err)
	}
	serverDBModel, schemaErrs := model.NewDatabaseModel(schema, dbModel)
	if len(schemaErrs) > 0 {
		t.Fatalf("schema errors: %v", schemaErrs)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	db := inmemory.NewDatabase(map[string]model.ClientDBModel{NorthboundDatabase: dbModel})
	srv, err := server.NewOvsdbServer(db, serverDBModel)
	if err != nil {
		t.Fatalf("starting test server: %v", err)
	}
	go func() { _ = srv.Serve("tcp", addr) }()
	for i := 0; i < 200 && !srv.Ready(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !srv.Ready() {
		t.Fatal("test OVSDB server never became ready")
	}

	client, err := New(context.Background(), Config{Endpoint: "tcp:" + addr})
	if err != nil {
		srv.Close()
		t.Fatalf("connecting test client: %v", err)
	}
	return client, func() {
		client.Close()
		srv.Close()
	}
}
