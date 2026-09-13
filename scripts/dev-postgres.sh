#!/usr/bin/env bash
# Throwaway Postgres for local dev and tests, no Docker needed.
# Data lives in ./.pgdata (gitignored) and the server listens on 55432 so it
# doesn't collide with a Postgres you already run on 5432.
#
#   scripts/dev-postgres.sh start|stop|reset
set -euo pipefail

DATA="${PGDATA_DIR:-$(pwd)/.pgdata}"
PORT="${FORGERAIL_PG_PORT:-55432}"

start() {
	if [ ! -f "$DATA/PG_VERSION" ]; then
		initdb -D "$DATA" -U postgres --auth=trust >/dev/null
	fi
	if ! pg_ctl -D "$DATA" status >/dev/null 2>&1; then
		pg_ctl -D "$DATA" -o "-p $PORT -k /tmp -c max_connections=200" -l "$DATA/server.log" -w start >/dev/null
	fi
	for db in forgerail forgerail_test; do
		createdb -h localhost -p "$PORT" -U postgres "$db" 2>/dev/null || true
	done
	echo "export DATABASE_URL=postgres://postgres@localhost:$PORT/forgerail?sslmode=disable"
	echo "export FORGERAIL_TEST_DATABASE_URL=postgres://postgres@localhost:$PORT/forgerail_test?sslmode=disable"
}

stop() {
	if [ -f "$DATA/PG_VERSION" ]; then
		pg_ctl -D "$DATA" -m fast stop >/dev/null 2>&1 || true
	fi
}

case "${1:-start}" in
start) start ;;
stop) stop ;;
reset)
	stop
	rm -rf "$DATA"
	start
	;;
*)
	echo "usage: $0 start|stop|reset" >&2
	exit 1
	;;
esac
