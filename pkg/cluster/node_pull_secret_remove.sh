oc get secret pull-secret -n openshift-config -o json \
  | jq '.data[".dockerconfigjson"] |= (@base64d | fromjson | del(.auths["quay.io/rhoai"]) | tojson | @base64)' \
  | oc replace -f -
