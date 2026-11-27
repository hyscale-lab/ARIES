supplemental_config() {
    local task=$1 arm=$2 attempt=$3 base=$4 metadata label name
    metadata=$(dirname "${BASH_SOURCE[0]}")/supplemental-sources.json
    IFS=$'\t' read -r label name source_sha < <(jq -er --arg t "$task" '.[$t] | [.label,.trace,.sha256] | @tsv' "$metadata")
    [[ -n "$source_sha" ]]
    service="$label-$arm-$attempt"; trace="$base/$name"
    cache="$base/image-cache"; [[ ! -d "$base/live/home/image-cache" ]] || cache="$base/live/home/image-cache"
}
supplemental_hosts() {
    mapfile -t hof_hosts < <(jq -er --arg t "$1" '.[$t].hosts[]' "$(dirname "${BASH_SOURCE[0]}")/supplemental-sources.json")
    (( ${#hof_hosts[@]} > 0 ))
    hof_hosts+=(brave-broker)
}
