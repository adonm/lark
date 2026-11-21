#!/usr/bin/env bash
set -euo pipefail
umask 077
data=${XDG_DATA_HOME:-$HOME/.local/share}/lark
dir=$data/photo-models
install -d -m 700 -- "$dir"
tmp=$(mktemp "$dir/.model.XXXXXXXX")
trap 'rm -f -- "$tmp"' EXIT
get() {
    local name=$1 sha=$2 url=$3
    if [[ -f "$dir/$name" ]] && printf '%s  %s\n' "$sha" "$dir/$name" | sha256sum --check --status; then
        return
    fi
    curl --fail --silent --show-error --location "$url" -o "$tmp"
    printf '%s  %s\n' "$sha" "$tmp" | sha256sum --check --status
    mv -fT -- "$tmp" "$dir/$name"
    tmp=$(mktemp "$dir/.model.XXXXXXXX")
}

zoo=https://media.githubusercontent.com/media/opencv/opencv_zoo/48b10fa505aeb51fbd127fc60a5c660040e823b7/models
clip=https://huggingface.co/Xenova/clip-vit-base-patch32/resolve/6b7c523703ae26e39be4b03774966f8571c7f523
get yunet.onnx 8f2383e4dd3cfbb4553ea8718107fc0423210dc964f9f4280604804ed2552fa4 "$zoo/face_detection_yunet/face_detection_yunet_2023mar.onnx"
get sface.onnx 0ba9fbfa01b5270c96627c4ef784da859931e02f04419c829e83484087c34e79 "$zoo/face_recognition_sface/face_recognition_sface_2021dec.onnx"
get vision_model_int8.onnx 0ab0c1b3ace708e539633af1744d5a95247fe4e14d3e08ff197ef82a6cb9bd93 "$clip/onnx/vision_model_int8.onnx"
get text_model_int8.onnx 18845f2ccc35223bb7fec403383a131154b11ac0918df25cf51986df5efd3a21 "$clip/onnx/text_model_int8.onnx"
get vocab.json 5047b556ce86ccaf6aa22b3ffccfc52d391ea4accdab9c2f2407da5b742d4363 "$clip/vocab.json"
get merges.txt 9fd691f7c8039210e0fced15865466c65820d09b63988b0174bfe25de299051a "$clip/merges.txt"
printf '%s\n' 'Installed checksum-verified local photo models. The bridge loads them on its next start.'
