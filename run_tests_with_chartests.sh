#!/usr/bin/env bash

# usage:
# ./run_tests.sh                         # local, go 1.12
# ./run_tests.sh docker                  # docker, go 1.12
# ./run_tests.sh podman                  # podman, go 1.12

set -ex

# The script does automatic checking on a Go package and its sub-packages,
# including:
# 1. gofmt         (http://golang.org/cmd/gofmt/)
# 2. go vet        (http://golang.org/cmd/vet)
# 3. gosimple      (https://github.com/dominikh/go-simple)
# 4. unconvert     (https://github.com/mdempsky/unconvert)
# 5. ineffassign   (https://github.com/gordonklaus/ineffassign)
# 6. race detector (http://blog.golang.org/race-detector)

# golangci-lint (github.com/golangci/golangci-lint) is used to run each each
# static checker.

# Default GOVERSION
[[ ! "$GOVERSION" ]] && GOVERSION=1.12
REPO=fnodata

testrepo () {
  TMPDIR=$(mktemp -d)
  TMPFILE=$(mktemp)
  export GO111MODULE=on

  go version

  ROOTPATH=$(go list -m -f {{.Dir}} 2>/dev/null)
  ROOTPATHPATTERN=$(echo $ROOTPATH | sed 's/\\/\\\\/g' | sed 's/\//\\\//g')
  MODPATHS=$(go list -m -f {{.Dir}} all 2>/dev/null | grep "^$ROOTPATHPATTERN")

  # Test application install
  go build
  (cd cmd/rebuilddb && go build)
  (cd cmd/rebuilddb2 && go build)
  (cd cmd/scanblocks && go build)
  (cd testutil/dbload && go build)

  mkdir -p ./testutil/dbload/testsconfig/test.data

  # Fetch the tests data.
  git clone https://github.com/fnolabs/bug-free-happiness $TMPDIR

  BLOCK_RANGE="0-199"

  tar xvf $TMPDIR/sqlitedb/sqlite_"$BLOCK_RANGE".tar.xz -C ./testutil/dbload/testsconfig/test.data
  tar xvf $TMPDIR/pgdb/pgsql_"$BLOCK_RANGE".tar.xz -C ./testutil/dbload/testsconfig/test.data
  tar xvf $TMPDIR/stakedb/test_ticket_pool.bdgr.tar.xz -C ./stakedb

  # Set up the tests db.
  psql -U postgres -c "DROP DATABASE IF EXISTS fnodata_mainnet_test"
  psql -U postgres -c "CREATE DATABASE fnodata_mainnet_test"

  # Pre-populate the pg db with test data.
  ./testutil/dbload/dbload

  # run tests on all modules
  for MODPATH in $MODPATHS; do
    env GORACE='halt_on_error=1' go test -v -race -tags chartests $(cd $MODPATH && go list -m)
  done

  # check linters
  ./lint.sh

  # Drop the tests db.
  psql -U postgres -c "DROP DATABASE IF EXISTS fnodata_mainnet_test"

  # webpack
  npm install
  npm run build

  echo "------------------------------------------"
  echo "Tests completed successfully!"

  # Remove all the tests data
  rm -rf $TMPDIR $TMPFILE
}

DOCKER=
[[ "$1" == "docker" || "$1" == "podman" ]] && DOCKER=$1
if [ ! "$DOCKER" ]; then
    testrepo
    exit
fi

DOCKER_IMAGE_TAG=fnodata-golang-builder-$GOVERSION
$DOCKER pull fonero/$DOCKER_IMAGE_TAG

$DOCKER run --rm -it -v $(pwd):/src fonero/$DOCKER_IMAGE_TAG /bin/bash -c "\
  rsync -ra --include-from=<(git --git-dir=/src/.git ls-files) \
  --filter=':- .gitignore' \
  /src/ /go/src/github.com/decred/$REPO/ && \
  cd github.com/decred/$REPO/ && \
  env GOVERSION=$GOVERSION GO111MODULE=on bash run_tests.sh"
