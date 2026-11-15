#!/usr/bin/env bash
# charts/glidepath-catalog/files/lib/oci-layout.sh - structural validation of an OCI image layout directory.
#
# The build pod (build-image, source-runner, runs the repository's Containerfile RUN lines) writes an OCI layout into the
# workspace. Everything in it is attacker-controlled data. push-image, the pod that holds the registry push credential,
# calls oci_layout_validate on it before it copies anything, and pushes only what this accepted. The layout is read as data
# (stat, wc, sha256, jq on size-capped JSON); nothing in it is executed, sourced or extracted.
#
#   oci_layout_validate DIR KIND WANT [ROOT]
#     DIR   the layout directory (not a symlink; with ROOT, no component from ROOT down to DIR is a symlink either)
#     KIND  image | cache
#     WANT  image: the exact platform set, "linux/amd64,linux/arm64" (comma separated, any order); cache: ignored
#   On success returns 0 and sets OCI_LAYOUT_DIGEST to the digest of the one descriptor in index.json (the index for several
#   platforms, the manifest for one), which this function has checked against the bytes on disk. On failure it prints one
#   "refusing the build output: <why>" line to stderr and returns 1.
#
# What is accepted, exactly:
#   - DIR contains only: oci-layout, index.json, blobs/, blobs/sha256/, blobs/sha256/<64 hex> (regular files) and, empty, ingest/.
#     No symlink, no device, no fifo, no other name (a name is matched whole, so no ".." and no separator tricks).
#   - oci-layout: {"imageLayoutVersion": "1.0.0"}. index.json: schemaVersion 2, exactly one manifest descriptor.
#   - every descriptor: sha256 digest, a size that equals the file's, content that hashes to the digest, no "urls".
#   - media types from the allowlists below, nothing else. Sizes bounded (env, with defaults):
#       OCI_MAX_TOTAL_BYTES (default 8 GiB, every distinct blob counted once), OCI_MAX_LAYERS (default 512 per manifest),
#       JSON documents at most 1 MiB (a cache config at most 64 MiB).
#   - image: one platform = a single manifest; several = one index (never nested) whose entries are exactly WANT, each linux,
#     none repeated, none extra (an attestation manifest, "unknown/unknown", is refused), each config's os/architecture equal
#     to its platform. cache: one manifest whose config is a buildkit cache config.
#
# Parser differential. This script reads JSON with jq; oras and registries read it with Go's encoding/json, which matches keys
# case-insensitively and lets the last duplicate win, while jq is case-sensitive. So a document is only accepted when (a) every
# object the consumers interpret has ONLY keys from an exact-name allowlist (a "Layers" beside "layers", or any other spelling or
# Unicode fold of an allowed name, is simply an unknown key and refuses), and (b) no object repeats a key. (b) is checked with
# `jq --stream`, the token-level view of the file, before jq's own (last-wins) parse is trusted: every leaf path must be unique and
# no path may be both a leaf and a container, and every non-empty container closes under a unique parent path (see _oci_nodups).
#
# Portable bash 3.2+ and busybox/GNU/BSD userland (no associative arrays, no find -printf).

OCI_MEDIA_INDEX="application/vnd.oci.image.index.v1+json"
OCI_MEDIA_MANIFEST="application/vnd.oci.image.manifest.v1+json"
OCI_MEDIA_CONFIG="application/vnd.oci.image.config.v1+json"
OCI_MEDIA_CACHE_CONFIG="application/vnd.buildkit.cacheconfig.v0"
OCI_MEDIA_LAYERS="application/vnd.oci.image.layer.v1.tar application/vnd.oci.image.layer.v1.tar+gzip application/vnd.oci.image.layer.v1.tar+zstd"

_oci_fail() {
  echo "refusing the build output: $*" >&2
  return 1
}

_oci_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum < "$1" | cut -d' ' -f1
  else
    shasum -a 256 < "$1" | cut -d' ' -f1
  fi
}

_oci_size() { wc -c < "$1" | tr -d ' '; }

# _oci_int NAME VALUE: the value is a non-negative integer of at most 15 digits
_oci_int() {
  case "$2" in
    ''|*[!0-9]*) return 1;;
  esac
  [ "${#2}" -le 15 ]
}

# _oci_check_blob DIR DIGEST SIZE MAXBYTES -> prints nothing; returns 1 with a message. Sets _OCI_BLOB (the path).
_oci_check_blob() {
  local dir="$1" digest="$2" size="$3" max="$4" hex actual
  case "${digest}" in
    sha256:*) hex="${digest#sha256:}";;
    *) _oci_fail "a descriptor has a digest that is not sha256 (${digest:0:20})"; return 1;;
  esac
  if ! _oci_name_ok "${hex}" '^[0-9a-f]{64}$'; then
    _oci_fail "a descriptor digest is not 64 hex characters"; return 1
  fi
  if ! _oci_int size "${size}"; then
    _oci_fail "a descriptor size is not a number"; return 1
  fi
  _OCI_BLOB="${dir}/blobs/sha256/${hex}"
  if [ -L "${_OCI_BLOB}" ] || [ ! -f "${_OCI_BLOB}" ]; then
    _oci_fail "blob sha256:${hex:0:12} is missing or not a regular file"; return 1
  fi
  actual="$(_oci_size "${_OCI_BLOB}")"
  if [ "${actual}" != "${size}" ]; then
    _oci_fail "blob sha256:${hex:0:12} is ${actual} bytes, its descriptor says ${size}"; return 1
  fi
  if [ "${size}" -gt "${max}" ]; then
    _oci_fail "blob sha256:${hex:0:12} is ${size} bytes, over the limit of ${max} bytes"; return 1
  fi
  if [ "$(_oci_sha256 "${_OCI_BLOB}")" != "${hex}" ]; then
    _oci_fail "blob sha256:${hex:0:12} does not hash to its digest"; return 1
  fi
  return 0
}

# _oci_account DIGEST SIZE: count a distinct blob once against OCI_MAX_TOTAL_BYTES
_oci_account() {
  case "${_OCI_SEEN}" in
    *"|$1|"*) return 0;;
  esac
  _OCI_SEEN="${_OCI_SEEN}|$1|"
  _OCI_TOTAL=$(( _OCI_TOTAL + $2 ))
  if [ "${_OCI_TOTAL}" -gt "${_OCI_MAX_TOTAL}" ]; then
    _oci_fail "the layout holds more than ${_OCI_MAX_TOTAL} bytes of blobs"; return 1
  fi
  return 0
}

# _oci_media_in MEDIA LIST...
_oci_media_in() {
  local m="$1" x
  shift
  for x in "$@"; do
    [ "${m}" = "${x}" ] && return 0
  done
  return 1
}

# _oci_json FILE FILTER: jq -e on a file already size-checked; only a fixed boolean filter, never printed content
_oci_json() { jq -e "$2" "$1" >/dev/null 2>&1; }

# _oci_nodups FILE: no object in the document repeats a key (jq would keep the last, Go's decoder also the last, but a different
# parser may keep the first). Over jq's token stream: leaf paths unique, closing parents unique, and no leaf path is also a container.
_oci_nodups() {
  jq -en --stream '[inputs] as $e
    | ($e | map(select(length == 2) | .[0])) as $leaves
    | ($e | map(select(length == 1) | .[0][:-1])) as $closes
    | (($leaves | length) == ($leaves | unique | length))
      and (($closes | length) == ($closes | unique | length))
      and ([$leaves[] as $l | $closes[] | select(. == $l)] | length == 0)' "$1" >/dev/null 2>&1
}

# _oci_keys FILE PATH ALLOWED...: every object selected by the jq PATH has only these exact keys
_oci_keys() {
  local f="$1" path="$2" allowed
  shift 2
  allowed="$(printf '%s\n' "$@" | jq -R . | jq -sc .)"
  jq -e --argjson a "${allowed}" "[(${path})? | objects | keys_unsorted[]] | all(. as \$k | \$a | index(\$k) != null)" "$f" >/dev/null 2>&1
}

# _oci_name_ok VALUE REGEX: the whole value matches (no line-wise matching: a newline never passes)
_oci_name_ok() {
  case "$1" in *$'\n'*|*$'\r'*) return 1;; esac
  [[ "$1" =~ $2 ]]
}

# no component from ROOT down to DIR (inclusive) is a symlink
_oci_path_clean() {
  local p="$1" root="$2"
  while :; do
    [ -L "${p}" ] && return 1
    [ "${p}" = "${root}" ] && return 0
    [ "${p}" = "/" ] || [ "${p}" = "." ] && return 0
    p="$(dirname "${p}")"
  done
}

# _oci_doc FILE DESC: a JSON document read by consumers: parseable, no duplicate keys
_oci_doc() {
  if ! _oci_nodups "$1"; then
    _oci_fail "$2 is not valid JSON or repeats a key"; return 1
  fi
}

_OCI_DESC_KEYS="mediaType digest size annotations"

# _oci_manifest DIR DIGEST SIZE KIND [PLATFORM]: validate one image manifest (config, layers); PLATFORM os/arch checks the config
_oci_manifest() {
  local dir="$1" digest="$2" size="$3" kind="$4" platform="${5:-}" mf cfg cfg_media want_cfg n max_layers l_media l_digest l_size
  _oci_check_blob "${dir}" "${digest}" "${size}" 1048576 || return 1
  mf="${_OCI_BLOB}"
  _oci_account "${digest}" "${size}" || return 1
  _oci_doc "${mf}" "a manifest" || return 1
  if ! _oci_json "${mf}" '.schemaVersion == 2 and (.config | type == "object") and (.layers | type == "array")'; then
    _oci_fail "a manifest is not a schema-2 image manifest"; return 1
  fi
  # exact key names only: a "Layers" next to "layers" is an unknown key, so it refuses
  if ! _oci_keys "${mf}" '.' schemaVersion mediaType config layers annotations ||
     ! _oci_keys "${mf}" '.config, .layers[]' ${_OCI_DESC_KEYS}; then
    _oci_fail "a manifest or one of its descriptors has a key that is not on the allowlist (or differs from one only by case)"; return 1
  fi
  if ! _oci_json "${mf}" ".mediaType == \"${OCI_MEDIA_MANIFEST}\""; then
    _oci_fail "a manifest does not declare the OCI manifest media type"; return 1
  fi
  cfg_media="$(jq -r '.config.mediaType // ""' "${mf}")"
  if [ "${kind}" = cache ]; then want_cfg="${OCI_MEDIA_CACHE_CONFIG}"; else want_cfg="${OCI_MEDIA_CONFIG}"; fi
  if [ "${cfg_media}" != "${want_cfg}" ]; then
    _oci_fail "a config has media type '${cfg_media:0:60}', expected ${want_cfg}"; return 1
  fi
  local cfg_max=1048576
  [ "${kind}" = cache ] && cfg_max=67108864
  _oci_check_blob "${dir}" "$(jq -r '.config.digest // ""' "${mf}")" "$(jq -r '.config.size // ""' "${mf}")" "${cfg_max}" || return 1
  cfg="${_OCI_BLOB}"
  _oci_account "$(jq -r '.config.digest' "${mf}")" "$(jq -r '.config.size' "${mf}")" || return 1
  _oci_doc "${cfg}" "a config" || return 1
  if ! _oci_json "${cfg}" 'type == "object"'; then
    _oci_fail "a config is not a JSON object"; return 1
  fi
  if [ "${kind}" = image ]; then
    # the fields container runtimes read from the image config, by exact name; nothing else at these levels
    if ! _oci_keys "${cfg}" '.' created author architecture os os.version os.features variant config rootfs history ||
       ! _oci_keys "${cfg}" '.config' User ExposedPorts Env Entrypoint Cmd Volumes WorkingDir Labels StopSignal ArgsEscaped Healthcheck Shell OnBuild ||
       ! _oci_keys "${cfg}" '.rootfs' type diff_ids ||
       ! _oci_keys "${cfg}" '.history[]' created created_by author comment empty_layer; then
      _oci_fail "an image config has a key that is not on the allowlist (or differs from one only by case)"; return 1
    fi
    if [ -n "${platform}" ] && [ "$(jq -r '(.os // "") + "/" + (.architecture // "")' "${cfg}")" != "${platform}" ]; then
      _oci_fail "an image config does not match its platform ${platform}"; return 1
    fi
  fi
  n="$(jq -r '.layers | length' "${mf}")"
  max_layers="${_OCI_MAX_LAYERS}"
  [ "${kind}" = cache ] && max_layers="${_OCI_MAX_CACHE_LAYERS}"
  if [ "${n}" -gt "${max_layers}" ]; then
    _oci_fail "a manifest has ${n} layers, over the limit of ${max_layers}"; return 1
  fi
  # one jq call for all layers; @tsv escapes tabs and newlines in values, so a value cannot add a field or a line
  while IFS=$'\t' read -r l_media l_digest l_size; do
    # shellcheck disable=SC2086
    if ! _oci_media_in "${l_media}" ${OCI_MEDIA_LAYERS}; then
      _oci_fail "a layer has media type '${l_media:0:60}', which is not an OCI tar layer"; return 1
    fi
    _oci_check_blob "${dir}" "${l_digest}" "${l_size}" "${_OCI_MAX_TOTAL}" || return 1
    _oci_account "${l_digest}" "${l_size}" || return 1
  done < <(jq -r '.layers[] | [(.mediaType // ""), (.digest // ""), ((.size // "") | tostring)] | @tsv' "${mf}")
  return 0
}

oci_layout_validate() {
  local dir="$1" kind="${2:-image}" want="${3:-}" root="${4:-}" p rel name top_media top_digest top_size idx n i platform seen plats w count plist
  OCI_LAYOUT_DIGEST=""
  _OCI_SEEN="|"
  _OCI_TOTAL=0
  _OCI_MAX_TOTAL="${OCI_MAX_TOTAL_BYTES:-8589934592}"
  _OCI_MAX_LAYERS="${OCI_MAX_LAYERS:-512}"
  # a mode=max cache holds a layer per stage step of every stage; BuildKit documents no cap, so this is only a sanity bound
  _OCI_MAX_CACHE_LAYERS="${OCI_MAX_CACHE_LAYERS:-8192}"
  _OCI_BLOB=""
  case "${kind}" in image|cache) ;; *) _oci_fail "unknown layout kind '${kind}'"; return 1;; esac
  if [ -L "${dir}" ] || [ ! -d "${dir}" ]; then
    _oci_fail "the layout directory is missing or is a symlink"; return 1
  fi
  if [ -n "${root}" ] && ! _oci_path_clean "${dir}" "${root}"; then
    _oci_fail "a component of the path to the layout is a symlink"; return 1
  fi
  # 1. nothing but regular files and plain directories, named exactly as the layout names them
  if [ -n "$(find "${dir}" ! -type f ! -type d -print 2>/dev/null | head -n 1)" ]; then
    _oci_fail "the layout contains a symlink, device, socket or fifo"; return 1
  fi
  while IFS= read -r -d '' p; do
    rel="${p#"${dir}"}"
    rel="${rel#/}"
    [ -z "${rel}" ] && continue
    case "${rel}" in
      oci-layout|index.json) [ -f "${p}" ] || { _oci_fail "${rel} is not a regular file"; return 1; };;
      blobs|blobs/sha256|ingest) [ -d "${p}" ] || { _oci_fail "${rel} is not a directory"; return 1; };;
      blobs/sha256/*)
        name="${rel#blobs/sha256/}"
        if ! _oci_name_ok "${name}" '^[0-9a-f]{64}$' || [ ! -f "${p}" ]; then
          _oci_fail "an entry under blobs/sha256 is not a 64-hex regular file"; return 1
        fi;;
      *) _oci_fail "the layout contains an unexpected entry"; return 1;;
    esac
  done < <(find "${dir}" -print0 2>/dev/null)
  if [ -d "${dir}/ingest" ] && [ -n "$(find "${dir}/ingest" -mindepth 1 -print 2>/dev/null | head -n 1)" ]; then
    _oci_fail "the layout's ingest directory is not empty"; return 1
  fi
  # 2. the marker and the one index
  [ -f "${dir}/oci-layout" ] && [ -f "${dir}/index.json" ] || { _oci_fail "oci-layout or index.json is missing"; return 1; }
  if [ "$(_oci_size "${dir}/oci-layout")" -gt 1024 ]; then
    _oci_fail "oci-layout is over 1 KiB"; return 1
  fi
  _oci_doc "${dir}/oci-layout" "oci-layout" || return 1
  if ! _oci_keys "${dir}/oci-layout" '.' imageLayoutVersion || ! _oci_json "${dir}/oci-layout" '.imageLayoutVersion == "1.0.0"'; then
    _oci_fail "oci-layout is not an OCI image layout 1.0.0 marker"; return 1
  fi
  if [ "$(_oci_size "${dir}/index.json")" -gt 1048576 ]; then
    _oci_fail "index.json is over 1 MiB"; return 1
  fi
  _oci_doc "${dir}/index.json" "index.json" || return 1
  if ! _oci_json "${dir}/index.json" '.schemaVersion == 2 and (.manifests | type == "array") and (.manifests | length == 1) and (.manifests[0] | type == "object")'; then
    _oci_fail "index.json is not a schema-2 index with exactly one manifest"; return 1
  fi
  if ! _oci_keys "${dir}/index.json" '.' schemaVersion mediaType manifests annotations ||
     ! _oci_keys "${dir}/index.json" '.manifests[]' mediaType digest size annotations platform ||
     ! _oci_keys "${dir}/index.json" '.manifests[].platform' architecture os variant; then
    _oci_fail "index.json has a key that is not on the allowlist (or differs from one only by case)"; return 1
  fi
  if _oci_json "${dir}/index.json" 'has("mediaType") and .mediaType != "application/vnd.oci.image.index.v1+json"'; then
    _oci_fail "index.json declares a media type other than the OCI index"; return 1
  fi
  top_media="$(jq -r '.manifests[0].mediaType // ""' "${dir}/index.json")"
  top_digest="$(jq -r '.manifests[0].digest // ""' "${dir}/index.json")"
  top_size="$(jq -r '.manifests[0].size // ""' "${dir}/index.json")"
  # 3. the one descriptor, by kind
  if [ "${kind}" = cache ]; then
    if [ "${top_media}" != "${OCI_MEDIA_MANIFEST}" ]; then
      _oci_fail "a cache layout must hold one OCI image manifest, not '${top_media:0:60}'"; return 1
    fi
    _oci_manifest "${dir}" "${top_digest}" "${top_size}" cache || return 1
    OCI_LAYOUT_DIGEST="${top_digest}"
    return 0
  fi
  plats="$(printf '%s' "${want}" | tr ',' '\n' | grep -v '^$' | sort -u)"
  count="$(printf '%s\n' "${plats}" | grep -c .)"
  if [ "${count}" -lt 1 ]; then
    _oci_fail "no expected platform was given"; return 1
  fi
  plist=" "
  for w in ${plats}; do
    case "${w}" in
      linux/amd64|linux/arm64) plist="${plist}${w} ";;
      *) _oci_fail "expected platform '${w}' is not one the platform builds"; return 1;;
    esac
  done
  case "${top_media}" in
    "${OCI_MEDIA_MANIFEST}")
      if [ "${count}" -ne 1 ]; then
        _oci_fail "the layout holds one image, ${count} platforms were requested"; return 1
      fi
      _oci_manifest "${dir}" "${top_digest}" "${top_size}" image "${plats}" || return 1;;
    "${OCI_MEDIA_INDEX}")
      _oci_check_blob "${dir}" "${top_digest}" "${top_size}" 1048576 || return 1
      idx="${_OCI_BLOB}"
      _oci_account "${top_digest}" "${top_size}" || return 1
      _oci_doc "${idx}" "the image index" || return 1
      if ! _oci_json "${idx}" '.schemaVersion == 2 and (.manifests | type == "array") and all(.manifests[]; type == "object")'; then
        _oci_fail "the image index is not a schema-2 index"; return 1
      fi
      if ! _oci_keys "${idx}" '.' schemaVersion mediaType manifests annotations ||
         ! _oci_keys "${idx}" '.manifests[]' mediaType digest size annotations platform ||
         ! _oci_keys "${idx}" '.manifests[].platform' architecture os variant; then
        _oci_fail "the image index has a key that is not on the allowlist (or differs from one only by case)"; return 1
      fi
      if _oci_json "${idx}" 'has("mediaType") and .mediaType != "application/vnd.oci.image.index.v1+json"'; then
        _oci_fail "the image index declares an unexpected media type"; return 1
      fi
      n="$(jq -r '.manifests | length' "${idx}")"
      if [ "${n}" -gt 8 ]; then
        _oci_fail "the image index holds ${n} images"; return 1
      fi
      seen=""
      i=0
      while [ "${i}" -lt "${n}" ]; do
        platform="$(jq -r ".manifests[${i}].platform | (.os // \"?\") + \"/\" + (.architecture // \"?\")" "${idx}")"
        if [ "$(jq -r ".manifests[${i}].mediaType // \"\"" "${idx}")" != "${OCI_MEDIA_MANIFEST}" ]; then
          _oci_fail "the image index lists an entry that is not an OCI image manifest (nested index or other type)"; return 1
        fi
        if ! _oci_name_ok "${platform}" '^[a-z0-9?]+/[a-z0-9?]+$'; then
          _oci_fail "the image index holds a malformed platform"; return 1
        fi
        case "${plist}" in
          *" ${platform} "*) ;;
          *) _oci_fail "the image index holds platform '${platform:0:40}', which was not requested"; return 1;;
        esac
        case "|${seen}|" in
          *"|${platform}|"*) _oci_fail "the image index lists platform ${platform} twice"; return 1;;
        esac
        seen="${seen}|${platform}"
        _oci_manifest "${dir}" "$(jq -r ".manifests[${i}].digest // \"\"" "${idx}")" "$(jq -r ".manifests[${i}].size // \"\"" "${idx}")" image "${platform}" || return 1
        i=$(( i + 1 ))
      done
      if [ "${n}" -ne "${count}" ]; then
        _oci_fail "the image index holds ${n} images, ${count} platforms were requested (${want})"; return 1
      fi;;
    *)
      _oci_fail "the layout's top descriptor has media type '${top_media:0:60}', not an OCI manifest or index"; return 1;;
  esac
  OCI_LAYOUT_DIGEST="${top_digest}"
  return 0
}
