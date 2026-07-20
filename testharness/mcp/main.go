// Command testharness-mcp is a tiny, deterministic MCP server backed by
// Postgres, for exercising the opentalon-agents stock watcher end-to-end.
//
// It exposes three tools over the legacy HTTP+SSE transport (which mcp-plugin
// connects to):
//
//   get_item(barcode)          -> {barcode, name, current_stock}
//   list_low_stock(threshold)  -> {items: [{barcode, current_stock}, ...]}
//   create_ticket(barcode,qty) -> {ticket_id, barcode, qty}
//
// All numeric args are declared and read as strings: the agents poll trigger
// passes args as map[string]string, so values arrive here as strings across
// the host -> mcp-plugin -> server chain.
//
// Env:
//   DATABASE_URL  postgres DSN (default postgres://<user>@localhost:5432/opentalon_test?sslmode=disable)
//   ADDR          listen address (default :8765); mcp-plugin points at http://<addr>/sse
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/user"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn())
	if err != nil {
		log.Fatalf("testharness-mcp: connect db: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("testharness-mcp: ping db (did you run seed.sql?): %v", err)
	}

	s := server.NewMCPServer("testharness", "0.1.0", server.WithToolCapabilities(true))

	s.AddTool(
		mcp.NewTool("get_item",
			mcp.WithDescription("Fetch a single inventory item by barcode."),
			mcp.WithString("barcode", mcp.Required(), mcp.Description("Item barcode, e.g. ABC-123")),
		),
		getItem(pool),
	)

	s.AddTool(
		mcp.NewTool("list_low_stock",
			mcp.WithDescription("List items whose current_stock is below a threshold."),
			mcp.WithString("threshold", mcp.Required(), mcp.Description("Integer threshold, e.g. 10")),
		),
		listLowStock(pool),
	)

	s.AddTool(
		mcp.NewTool("create_ticket",
			mcp.WithDescription("Open a refill ticket for an item."),
			mcp.WithString("barcode", mcp.Required(), mcp.Description("Item barcode")),
			mcp.WithString("qty", mcp.Required(), mcp.Description("Refill quantity, integer")),
		),
		createTicket(pool),
	)

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8765"
	}
	sse := server.NewSSEServer(s)
	log.Printf("testharness-mcp: listening on %s (SSE endpoint %s/sse)", addr, addr)
	if err := sse.Start(addr); err != nil {
		log.Fatalf("testharness-mcp: serve: %v", err)
	}
}

func dsn() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	name := "postgres"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return fmt.Sprintf("postgres://%s@localhost:5432/opentalon_test?sslmode=disable", name)
}

func getItem(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		barcode, err := req.RequireString("barcode")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var (
			name  string
			stock int
		)
		err = pool.QueryRow(ctx,
			`SELECT name, current_stock FROM items WHERE barcode = $1`, barcode,
		).Scan(&name, &stock)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("get_item %s: %v", barcode, err)), nil
		}
		log.Printf("testharness-mcp: get_item %s -> stock %d", barcode, stock)
		return jsonResult(map[string]any{
			"barcode":       barcode,
			"name":          name,
			"current_stock": stock,
		})
	}
}

func listLowStock(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		threshold, err := intArg(req, "threshold")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		rows, err := pool.Query(ctx,
			`SELECT barcode, current_stock FROM items WHERE current_stock < $1 ORDER BY barcode`, threshold)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list_low_stock: %v", err)), nil
		}
		defer rows.Close()

		items := []map[string]any{}
		for rows.Next() {
			var (
				barcode string
				stock   int
			)
			if err := rows.Scan(&barcode, &stock); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("list_low_stock scan: %v", err)), nil
			}
			items = append(items, map[string]any{"barcode": barcode, "current_stock": stock})
		}
		if err := rows.Err(); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("list_low_stock rows: %v", err)), nil
		}
		return jsonResult(map[string]any{"items": items})
	}
}

func createTicket(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		barcode, err := req.RequireString("barcode")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		qty, err := intArg(req, "qty")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var id int
		err = pool.QueryRow(ctx,
			`INSERT INTO tickets (barcode, qty) VALUES ($1, $2) RETURNING id`, barcode, qty,
		).Scan(&id)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("create_ticket %s: %v", barcode, err)), nil
		}
		log.Printf("testharness-mcp: created ticket %d for %s qty %d", id, barcode, qty)
		return jsonResult(map[string]any{"ticket_id": id, "barcode": barcode, "qty": qty})
	}
}

// intArg reads a required arg as a string and parses it, tolerating the
// string-typed args the agents poll trigger sends.
func intArg(req mcp.CallToolRequest, key string) (int, error) {
	s, err := req.RequireString(key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %q", key, s)
	}
	return n, nil
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}
