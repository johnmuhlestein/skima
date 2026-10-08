#!/bin/sh
# NOTE: this is just a wrapper script that'll wait for the istio sidecar to be running before the app
# executes. After the execution is complete, it tells the istio sidecar to shut down. This allows k8s
# Jobs to complete

if [ "${VAULT_AUTO_INJECT}" = "true" ]; then
  for f in /vault/secrets/*.env; do
    # with no matches the glob is left unexpanded, so skip anything that isn't a real file
    if [ ! -f "$f" ]; then
      echo "no vault env files found in /vault/secrets"
      continue
    fi
    echo "sourcing file: $f"
    # auto-export everything sourced so the values reach the app process
    set -a
    . "$f"
    set +a
  done
else
  echo "not injecting vault"
fi

if [ -n "${ISTIO_STATUS}" ]; then
  # Make sure the istio sidecar is up before running the application
  until curl -fsI http://localhost:15021/healthz/ready > /dev/null; do
    echo "Waiting for Istio Sidecar..."
    sleep 3
  done
fi

echo "Running app..."
# The args should be the intended command
"$@"
EXIT_CODE=$?
echo "App execution complete with exit code $EXIT_CODE"

if [ -n "${ISTIO_STATUS}" ]; then
  # Tell the istio sidecar to shut down
  echo "Shutting down Istio Sidecar..."
  curl -fs -X POST http://localhost:15020/quitquitquit
  echo "Istio Sidecar shutdown request sent"
fi
exit $EXIT_CODE
