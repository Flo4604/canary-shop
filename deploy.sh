#!/usr/bin/env bash
set +x
set -euo pipefail
umask 077

mode="${1:-deploy}"
[[ "$#" -le 1 && ( "$mode" == deploy || "$mode" == --diagnose-project || "$mode" == --repair-worker-url || "$mode" == --check-deployments ) ]] || {
  echo 'Usage: ./deploy.sh [--diagnose-project|--repair-worker-url|--check-deployments]' >&2
  exit 1
}

for tool in curl jq openssl; do
  command -v "$tool" >/dev/null || { echo "Missing command: $tool" >&2; exit 1; }
done

export UNKEY_BASE_URL="${UNKEY_BASE_URL:-https://api.unkey-canary.com}"
UNKEY_BASE_URL="${UNKEY_BASE_URL%/}"
if [[ "$UNKEY_BASE_URL" != https://api.unkey-canary.com ]]; then
  echo 'This script only deploys to https://api.unkey-canary.com.' >&2
  exit 1
fi
if [[ -z "${UNKEY_ROOT_KEY:-}" ]]; then
  read -rsp 'Canary root key (used for deployment and both apps): ' UNKEY_ROOT_KEY
  echo
fi
if [[ "$mode" == deploy && -z "${STOREFRONT_API_ID:-}" ]]; then
  read -rp 'Storefront API ID from init: ' STOREFRONT_API_ID
fi
if [[ "$mode" == deploy && -z "${WAREHOUSE_API_ID:-}" ]]; then
  read -rp 'Warehouse API ID from init: ' WAREHOUSE_API_ID
fi
[[ -n "$UNKEY_ROOT_KEY" && "$UNKEY_ROOT_KEY" != *$'\n'* && "$UNKEY_ROOT_KEY" != *$'\r'* ]] || exit 1
if [[ "$mode" == deploy ]] && ! [[ "$STOREFRONT_API_ID" =~ ^api_[a-zA-Z0-9_]+$ && "$WAREHOUSE_API_ID" =~ ^api_[a-zA-Z0-9_]+$ && "$STOREFRONT_API_ID" != "$WAREHOUSE_API_ID" ]]; then
  echo 'Provide two distinct API IDs.' >&2
  exit 1
fi
export UNKEY_ROOT_KEY STOREFRONT_API_ID WAREHOUSE_API_ID
export SHOP_WORKER_TOKEN
SHOP_WORKER_TOKEN="$(openssl rand -hex 32)"

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
printf 'Authorization: Bearer %s\nContent-Type: application/json\n' "$UNKEY_ROOT_KEY" > "$scratch/headers"

request() {
  local operation="$1" body="$2" status
  status="$(printf '%s' "$body" | curl --silent --show-error --max-time 60 \
    --header "@$scratch/headers" --data-binary @- \
    --output "$scratch/response" --write-out '%{http_code}' \
    "$UNKEY_BASE_URL/v2/$operation")"
  if [[ ! "$status" =~ ^2[0-9][0-9]$ ]]; then
    echo "$operation failed: HTTP $status. Writes are not retried." >&2
    jq -Rs -r 'split(env.UNKEY_ROOT_KEY) | join("[REDACTED]")' "$scratch/response" >&2
    return 1
  fi
  jq -e 'has("data") and .data != null' "$scratch/response" >/dev/null
}

if [[ "$mode" == --diagnose-project ]]; then
  echo 'Read-only check: looking up canary-shop. No resources will be created.'
  request projects.getProject '{"project":"canary-shop"}'
  jq '{id:.data.id,slug:.data.slug}' "$scratch/response"
  echo 'Project exists. Do not rerun project creation.'
  exit 0
fi

deploy() {
  local app="$1" environment="${2:-production}" branch="${3:-main}" id state attempt
  request deployments.createDeployment "$(jq -nc --arg app "$app" --arg environment "$environment" --arg branch "$branch" \
    '{project:"canary-shop",app:$app,environment:$environment,git:{branch:$branch}}')"
  id="$(jq -er '.data.deploymentId' "$scratch/response")"
  if [[ "$environment" == preview ]]; then
    preview_id="$id"
  fi
  echo "$app deployment: $id"
  for attempt in {1..180}; do
    request deployments.getDeployment "$(jq -nc --arg id "$id" '{deploymentId:$id}')"
    state="$(jq -er '.data.status' "$scratch/response")"
    echo "$app: $state"
    case "$state" in
      ready) return 0 ;;
      failed|skipped|superseded|stopped|cancelled|awaiting_approval)
        echo "Inspect deployment $id in the canary dashboard." >&2
        return 1 ;;
    esac
    sleep 10
  done
  echo "Timed out waiting for $id. Deployment may still be running; do not blindly rerun." >&2
  return 1
}

if [[ "$mode" == --check-deployments ]]; then
  command -v git >/dev/null
  echo 'This deploys main to the shop, pushes a new demo preview branch, checks routing, and stops that preview.'
  echo 'The production worker is not changed. GitHub branches are kept for inspection.'
  read -rp 'Continue? [y/N] ' confirmation
  [[ "$confirmation" == y || "$confirmation" == Y ]] || exit 0
  git clone --quiet --single-branch --branch main https://github.com/Flo4604/canary-shop.git "$scratch/source"
  main_commit="$(git -C "$scratch/source" rev-parse HEAD)"
  main_version="$(sed -n 's/^ARG APP_VERSION=//p' "$scratch/source/Dockerfile")"
  [[ -n "$main_version" ]] || { echo 'Publish the version endpoint changes to main first.' >&2; exit 1; }
  branch="canary-preview-$(date -u +%Y%m%d%H%M%S)-$(openssl rand -hex 3)"
  git -C "$scratch/source" switch --quiet -c "$branch"
  sed -i "s/^ARG APP_VERSION=.*/ARG APP_VERSION=$branch/" "$scratch/source/Dockerfile"
  git -C "$scratch/source" add Dockerfile
  git -C "$scratch/source" commit --quiet -m 'canary: mark preview deployment for routing checks'

  preview_id=''
  cleanup_preview() {
    local exit_status=$? state action attempt
    trap - EXIT
    if [[ -n "$preview_id" ]]; then
      if request deployments.getDeployment "$(jq -nc --arg id "$preview_id" '{deploymentId:$id}')"; then
        state="$(jq -r '.data.status' "$scratch/response")"
        if [[ "$state" != stopped && "$state" != failed && "$state" != cancelled && "$state" != superseded && "$state" != skipped ]]; then
          action="$(jq -r 'if (.data.availableActions | index("stop")) != null then "stopDeployment" elif (.data.availableActions | index("cancel")) != null then "cancelDeployment" else empty end' "$scratch/response")"
          if [[ -z "$action" ]] || ! request "deployments.$action" "$(jq -nc --arg id "$preview_id" '{deploymentId:$id}')"; then
            echo "CLEANUP REQUIRED: stop preview $preview_id in the dashboard." >&2
            exit_status=1
          else
            echo "Preview cleanup requested: $preview_id"
            for attempt in {1..60}; do
              if ! request deployments.getDeployment "$(jq -nc --arg id "$preview_id" '{deploymentId:$id}')"; then
                exit_status=1
                break
              fi
              state="$(jq -r '.data.status' "$scratch/response")"
              [[ "$state" == stopped || "$state" == cancelled ]] && break
              sleep 5
            done
            if [[ "$state" != stopped && "$state" != cancelled ]]; then
              echo "CLEANUP REQUIRED: preview $preview_id has not stopped." >&2
              exit_status=1
            else
              echo "Preview cleanup confirmed: $state"
            fi
          fi
        fi
      else
        echo "CLEANUP REQUIRED: inspect preview $preview_id in the dashboard." >&2
        exit_status=1
      fi
    fi
    rm -rf "$scratch"
    exit "$exit_status"
  }
  trap cleanup_preview EXIT

  for environment in production preview; do
    request environments.updateSettings "$(jq -nc --arg environment "$environment" \
      '{project:"canary-shop",app:"shop",environment:$environment,autoDeploy:false,dockerfile:"Dockerfile",port:8080,command:["/usr/local/bin/canary-shop","serve","--daily-requests","10000"],healthcheck:{method:"GET",path:"/healthz",intervalSeconds:10,timeoutSeconds:5,failureThreshold:3,initialDelaySeconds:10}}')"
  done
  request environments.setEnvironmentVariables "$(jq -nc \
    '{project:"canary-shop",app:"shop",environment:"preview",variables:[{key:"UNKEY_BASE_URL",value:env.UNKEY_BASE_URL},{key:"UNKEY_ROOT_KEY",value:env.UNKEY_ROOT_KEY},{key:"SHOP_WORKER_TOKEN",value:env.SHOP_WORKER_TOKEN}],prune:false}')"
  git -C "$scratch/source" push origin "$branch"

  check_domains() {
    local file="$1" expected="$2" domain
    jq -e '.data.domains | length > 0' "$file" >/dev/null
    while IFS= read -r domain; do
      if ! curl --silent --show-error --fail --max-time 30 "https://${domain#https://}/version" | jq -e --arg expected "$expected" '.version == $expected' >/dev/null; then
        echo "Wrong version or unreachable route: $domain (expected $expected)" >&2
        return 1
      fi
      echo "Route verified: $domain -> $expected"
    done < <(jq -r '.data.domains[]' "$file")
  }

  deploy shop production main
  cp "$scratch/response" "$scratch/production"
  jq -e --arg commit "$main_commit" '.data.git.commitSha == $commit and .data.environment == "production" and .data.git.branch == "main"' "$scratch/production" >/dev/null
  production_id="$(jq -er '.data.id' "$scratch/production")"
  check_domains "$scratch/production" "$main_version"
  deploy shop preview "$branch"
  jq -e --arg branch "$branch" '.data.environment == "preview" and .data.git.branch == $branch' "$scratch/response" >/dev/null
  cp "$scratch/response" "$scratch/preview"
  check_domains "$scratch/preview" "$branch"
  check_domains "$scratch/production" "$main_version"
  request apps.getApp '{"project":"canary-shop","app":"shop"}'
  jq -e --arg id "$production_id" '.data.currentDeploymentId == $id' "$scratch/response" >/dev/null
  echo 'PASS: production and preview routes serve different builds; preview did not replace production.'
  exit 0
fi

if [[ "$mode" == --repair-worker-url ]]; then
  request apps.getApp '{"project":"canary-shop","app":"shop"}'
  shop_deployment="$(jq -er '.data.currentDeploymentId | select(type == "string" and length > 0)' "$scratch/response")"
  request deployments.getDeployment "$(jq -nc --arg id "$shop_deployment" '{deploymentId:$id}')"
  jq -e '.data.status == "ready"' "$scratch/response" >/dev/null || {
    echo 'Shop is not ready; worker was not changed.' >&2
    exit 1
  }
  domain="$(jq -er '.data.domains[0] | select(type == "string" and length > 0)' "$scratch/response")"
  export SHOP_URL="https://${domain#https://}"
  curl --silent --show-error --fail --max-time 30 "$SHOP_URL/healthz" | jq -e '.code == "ALIVE"' >/dev/null
  request environments.setEnvironmentVariables "$(jq -nc \
    '{project:"canary-shop",app:"worker",environment:"production",variables:[{key:"SHOP_URL",value:env.SHOP_URL}],prune:false}')"
  echo "Worker SHOP_URL set to $SHOP_URL. Other variables were preserved."
  deploy worker
  echo 'Check worker runtime logs for scenario complete after the 30-second startup wait.'
  exit 0
fi

echo 'This creates a new canary-shop project and deploys two apps on Unkey.'
echo 'One root key is stored in both apps. Use a dedicated canary workspace key, never a production key.'
echo 'The Unkey GitHub integration must have access to Flo4604/canary-shop.'
read -rp 'Continue? [y/N] ' confirmation
[[ "$confirmation" == y || "$confirmation" == Y ]] || exit 0

request projects.createProject '{"name":"Canary Shop","slug":"canary-shop"}'
for app in shop worker; do
  request apps.createApp "$(jq -nc --arg app "$app" \
    '{project:"canary-shop",name:$app,slug:$app,git:{repository:"Flo4604/canary-shop",defaultBranch:"main"}}')"
  for environment in production preview; do
    request environments.updateSettings "$(jq -nc --arg app "$app" --arg environment "$environment" \
      '{project:"canary-shop",app:$app,environment:$environment,autoDeploy:false,dockerfile:"Dockerfile"}')"
  done
done

request environments.updateSettings '{"project":"canary-shop","app":"shop","environment":"production","port":8080,"command":["/usr/local/bin/canary-shop","serve","--daily-requests","10000"],"healthcheck":{"method":"GET","path":"/healthz","intervalSeconds":10,"timeoutSeconds":5,"failureThreshold":3,"initialDelaySeconds":10}}'
request environments.updateSettings '{"project":"canary-shop","app":"worker","environment":"production","command":["/usr/local/bin/canary-shop","worker","--interval","3s"],"healthcheck":null}'

for app in shop worker; do
  request environments.setEnvironmentVariables "$(jq -nc --arg app "$app" \
    '{project:"canary-shop",app:$app,environment:"production",variables:[
      {key:"UNKEY_BASE_URL",value:env.UNKEY_BASE_URL},
      {key:"UNKEY_ROOT_KEY",value:env.UNKEY_ROOT_KEY},
      {key:"SHOP_WORKER_TOKEN",value:env.SHOP_WORKER_TOKEN},
      {key:"STOREFRONT_API_ID",value:env.STOREFRONT_API_ID},
      {key:"WAREHOUSE_API_ID",value:env.WAREHOUSE_API_ID}
    ]}')"
done

deploy shop
domain="$(jq -er '.data.domains[0] | select(type == "string" and length > 0)' "$scratch/response")"
export SHOP_URL="https://$domain"
curl --silent --show-error --fail --max-time 30 "$SHOP_URL/healthz" | jq -e '.code == "ALIVE"' >/dev/null
request environments.setEnvironmentVariables "$(jq -nc \
  '{project:"canary-shop",app:"worker",environment:"production",variables:[{key:"SHOP_URL",value:env.SHOP_URL}]}')"
deploy worker

echo "Shop: $SHOP_URL"
echo 'Worker deployed. Check runtime logs for scenario complete after the 30-second startup wait.'
echo 'Traffic runs on Unkey. Keep one worker; stop it before future redeployments.'
