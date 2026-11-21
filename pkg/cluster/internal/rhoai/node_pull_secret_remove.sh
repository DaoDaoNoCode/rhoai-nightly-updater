printf 'This removes the quay.io/rhoai entry of openshift-config/pull-secret, whoever added it: also one the cluster was set up with, whose token the updater cannot restore. Type yes to remove it: ' && read -r ANSWER && [ "$ANSWER" = yes ] && oc get secret pull-secret -n openshift-config -o json \
  | jq '.data[".dockerconfigjson"] |= (@base64d | fromjson | del(.auths["quay.io/rhoai"]) | tojson | @base64)' \
  | oc replace -f -
unset ANSWER
