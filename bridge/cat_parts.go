package main

import "slices"

var faceParts = []string{"eyes", "mouth", "nose", "whiskers", "cheeks", "forehead"}
var faceEditPoses = []string{"base", "idle", "blink", "talk", "listen", "think", "sleep"}

func editFaceParts(raw string, edits any) (string, error) {
	list, ok := edits.([]any)
	if !ok || len(list) == 0 || len(list) > 24 {
		return "", fieldError("cat.face_edits", "expected 1–24 face part edits")
	}
	root, err := parseFaceSVG(raw)
	if err != nil {
		return "", problem(400, err.Error())
	}
	type partTarget struct {
		part, pose string
		node       *svgNode
	}
	var parts []partTarget
	var find func(*svgNode, string)
	find = func(n *svgNode, pose string) {
		if tag := n.attr["data-pose"]; tag != "" {
			pose = tag
		}
		if part := n.attr["data-part"]; part != "" {
			parts = append(parts, partTarget{part, pose, n})
			return
		}
		for _, child := range n.children {
			find(child, pose)
		}
	}
	find(root, "base")
	if len(parts) == 0 {
		return "", fieldError("cat.face_edits", "untagged custom face; send the complete face_svg instead")
	}
	updated := map[*svgNode]bool{}
	for _, raw := range list {
		edit, ok := raw.(object)
		if !ok {
			return "", fieldError("cat.face_edits", "expected a part edit object")
		}
		part, pose := str(edit["part"]), str(edit["pose"])
		_, poseGiven := edit["pose"]
		if !slices.Contains(faceParts, part) || poseGiven && !slices.Contains(faceEditPoses, pose) {
			return "", fieldError("cat.face_edits", "unknown part or pose")
		}
		svg, ok := edit["svg"].(string)
		if !ok {
			return "", fieldError("cat.face_edits.svg", "expected an SVG fragment")
		}
		replacement, err := parseFaceSVG("<svg>" + svg + "</svg>")
		if err != nil {
			return "", fieldError("cat.face_edits.svg", "%s", err)
		}
		var rejectMarkers func(*svgNode) bool
		rejectMarkers = func(n *svgNode) bool {
			return n.attr["data-pose"] != "" || n.attr["data-part"] != "" || slices.ContainsFunc(n.children, rejectMarkers)
		}
		if rejectMarkers(replacement) {
			return "", fieldError("cat.face_edits.svg", "replacement cannot define pose or part markers")
		}
		found := false
		for _, target := range parts {
			if target.part != part || poseGiven && target.pose != pose {
				continue
			}
			if updated[target.node] {
				return "", fieldError("cat.face_edits", "multiple edits target %s in %s", part, target.pose)
			}
			target.node.children = replacement.children
			updated[target.node] = true
			found = true
		}
		if !found {
			return "", fieldError("cat.face_edits", "no matching %s in requested pose; use face_svg to add parts", part)
		}
	}
	return sanitizeFaceSVG(svgString(root))
}
