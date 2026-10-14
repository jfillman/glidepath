#!/usr/bin/env bash
# catalog/lib/chain-slug.sh
#
# chain_id_to_slug alone, split out of cdevents.sh (2026-09-16) so a Task that only
# wants the human-friendly slug - not to actually emit a CDEvent - doesn't have to
# source cdevents.sh and satisfy its hard CDEVENTS_BROKER_URL/NAMESPACE/
# TEKTON_PIPELINE_RUN requirements (`: "${VAR:?...}"`, under `set -euo pipefail`) just
# to call one pure function. cdevents.sh now sources this file itself, so cdevent_send's
# own chain_slug computation is unchanged - one definition, not two copies drifting
# apart.

set -euo pipefail

# Word lists for chain_slug (see chain_id_to_slug below) - a human-friendly stand-in
# for the raw chain-id UUID in PipelineRun names. Exactly 256 words each, so one hex
# byte of the chain-id picks each word with no modulo bias: 65,536 slugs. The first
# lists (2026-09-05) had 39 adjectives and 40 nouns with heron and egret listed twice,
# about 1,500 distinct slugs, so an app's slugs repeated within weeks - and Tower
# correlates a flow's runs by slug. Rules, checked by hack/ci/chain_slug_test.sh:
# 256 entries each, no duplicates, no word in both lists, 2-6 lowercase letters (the
# slug is part of a 63-char DNS-1123 PipelineRun name). Order is the mapping - append
# or reorder only together with Tower's mirror (tower src/tekton/chainSlug.ts).
_CHAIN_SLUG_ADJECTIVES=(
  able agile airy alert alive amber ample apt ardent arid astral avid azure balmy beige
  bold brave breezy bright brisk bronze bubbly busy calm candid canny cheery chief
  chilly chirpy civil clean clear clever cloudy cobalt comfy cool copper cosmic cozy
  crisp curly cushy cyan dainty dapper daring dawn deep deft dewy direct divine dreamy
  dusky dusty early eager easy elated elfin epic equal even exact fair famed fancy fast
  fiery fine firm first fleet fluent fond frank free fresh frisky frosty gentle giant
  gifted glad glossy golden good grand great green handy happy hardy hazy hearty hefty
  heroic honest humble icy ideal indigo inky ivory jade jaunty jazzy jolly jovial joyful
  jumbo just keen kind kingly lavish leafy level light lilac limber lithe lively loyal
  lucid lucky lunar lush magic modern mellow merry mighty mild minty misty modest mossy
  neat nimble noble nifty oaken ochre olden open ornate pastel peppy perky placid plain
  plucky plush polar polite prime proud pure quaint quick quiet rapid rare ready regal
  rosy royal ruddy rugged russet rustic salty sandy savvy scenic serene sharp shiny
  silky silver simple sleek smart smooth snappy snowy snug solar solid sonic spry steady
  steely still stoic stout sturdy suave subtle sunny super supple sure tawny tender tidy
  tiny tough true trusty umber upbeat urban vast velvet vernal vital vivid warm wavy
  wild windy winged wise witty woody young zany zesty zippy agate aglow alpine arctic
  auburn blithe bonny bouncy brainy choice coral crafty dandy dulcet fabled feisty lofty
  gusty sunlit starry astute hushed cheeky
)
_CHAIN_SLUG_NOUNS=(
  heron finch robin swan crane stork ibis egret quail grebe plover osprey falcon kite
  jay hawk crow raven eagle owl wren lark dove tern gull petrel puffin auk loon rail
  coot snipe curlew dunlin knot sora vireo oriole thrush magpie rook pipit linnet siskin
  towhee junco martin merlin condor parrot macaw kea toucan myna bulbul shrike avocet
  godwit gannet skua fulmar teal wigeon eider scoter goose brant rhea emu kiwi trogon
  motmot jacana hoopoe drongo weaver canary serin grouse chukar otter fox wolf lynx bear
  seal moose bison puma civet mole vole whale badger beaver marten ferret stoat weasel
  mink ermine hare rabbit pika marmot gopher lemur loris gibbon baboon okapi zebra camel
  llama alpaca vicuna tapir rhino hippo eland kudu impala ibex oryx gnu yak dingo coyote
  jackal hyena dhole fennec serval ocelot margay jaguar cougar panda koala wombat numbat
  quokka possum dugong walrus orca beluga sloth genet coati skunk deer elk bongo nyala
  gaur zebu takin saiga goral tahr bobcat tiger lion salmon trout perch pike carp tuna
  marlin bass cod hake sole skate ray eel shark manta squid krill crab prawn shrimp
  oyster clam mussel urchin wrasse tetra guppy minnow tarpon mako wahoo snook bream
  barbel loach newt toad gecko viper cobra python mamba adder skink iguana anole turtle
  caiman gavial frog beetle moth bee wasp hornet cicada mantis mayfly locust scarab pony
  foal colt lamb stag doe boar ram ox bat dipper dodo ouzel chough crake ruff smew
  agouti paca mara degu jerboa gerbil shrew sable duiker dikdik mullet goby blenny smelt
)

# chain_id_to_slug <chain-id-uuid>
#   Deterministic two-word slug from a chain-id's first 4 hex chars (the UUID's
#   time_low field - no RFC4122 version/variant bits fixed there, so no encoding bias
#   worth avoiding): byte 0 picks the adjective, byte 1 the noun. The modulo is a guard
#   for a list that ever stops being 256 long, not part of the mapping. Same chain-id always yields the same slug, so every PipelineRun in
#   one flow chain shows the same words, letting them be visually grouped without
#   needing the full chain-id.
chain_id_to_slug() {
  local chain_id="$1"
  local adj_idx noun_idx
  adj_idx=$(( 16#${chain_id:0:2} % ${#_CHAIN_SLUG_ADJECTIVES[@]} ))
  noun_idx=$(( 16#${chain_id:2:2} % ${#_CHAIN_SLUG_NOUNS[@]} ))
  printf '%s-%s' "${_CHAIN_SLUG_ADJECTIVES[$adj_idx]}" "${_CHAIN_SLUG_NOUNS[$noun_idx]}"
}
