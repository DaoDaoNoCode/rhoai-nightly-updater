printf 'quay.io/rhoai token (base64 user:password from step 1, with its = padding): ' && read -rs QUAY_AUTH && echo
[ -n "$QUAY_AUTH" ] && oc get secret pull-secret -n openshift-config -o json \
  | QUAY_AUTH="$QUAY_AUTH" jq '.data[".dockerconfigjson"] |= (@base64d | fromjson | .auths["quay.io/rhoai"] = {auth: $ENV.QUAY_AUTH} | tojson | @base64)' \
  | oc replace -f -
unset QUAY_AUTH
