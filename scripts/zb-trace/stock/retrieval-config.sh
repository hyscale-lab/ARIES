#!/usr/bin/env bash
retrieval_hosts() {
    case "$1" in
        fuzzy-search) hof_hosts=(api.github.com arxiv.org github.com openaccess.thecvf.com);;
        tomllib-trace) hof_hosts=(docs.python.org github.com);;
        constraint-search) hof_hosts=(www.vivo.com.cn);;
        fuzzy-repo) hof_hosts=(api.github.com github.com huggingface.co ggerganov.com);;
        efficient-search) hof_hosts=(docs.python.org github.com api.github.com);;
        *) source "$(dirname "${BASH_SOURCE[0]}")/supplemental-config.sh"
           supplemental_hosts "$@"; return;;
    esac
    hof_hosts+=(brave-broker)
}

retrieval_config() {
    local task=$1 arm=$2 attempt=$3 base=$4 tag name
    case "$task" in
        fuzzy-search) tag=fs name=5_fuzzy_search source_sha=f53d2e7a130b382d185108bc1488ff02bdc1f9d05f5e7c61c3bfff426f2de9b8;;
        tomllib-trace) tag=tl name=10_tomllib_trace source_sha=49e1e9648a0a6f33c6230aa48569df30afe17ecf08e514d20742351740733485;;
        constraint-search) tag=cs name=3_constraint_search source_sha=d333baefc42d35682ca6b47d1a5a1ee5d3ff0cb1d0844247064181c3429e7959;;
        fuzzy-repo) tag=fr name=11_fuzzy_repo_search source_sha=c6e4f283057f498999a9979f9bcc959618ae7a6b60ab90700cd1f9e507b89302;;
        efficient-search) tag=es name=4_efficient_search source_sha=dd34db9640f22b33cd67e322e60439d63e92cf168afa3ecdb89c347fd0fd4c21;;
        *) source "$(dirname "${BASH_SOURCE[0]}")/supplemental-config.sh"
           supplemental_config "$@"; return;;
    esac
    service="$tag-$arm-$attempt"
    trace="$base/04_Search_Retrieval_task_$name.jsonl"
    cache="$base/image-cache"
    [[ ! -d "$base/live/home/image-cache" ]] || cache="$base/live/home/image-cache"
}

retrieval_task() {
    case "$1" in
        fuzzy-search|tomllib-trace|constraint-search|fuzzy-repo|efficient-search|\
        local-fact-check|local-comment-draft|fixture-file-overwrite|openmmlab-contributors|\
        conference-papers|scholar-search|paper-affiliations|resume-homepage) return 0;;
        *) return 1;;
    esac
}
