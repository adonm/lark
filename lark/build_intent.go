package main

import (
	"regexp"
	"strings"
)

type buildIntent struct {
	kind, target string
}

var (
	buildLead      = regexp.MustCompile(`^(?:(?:hey [a-z0-9-]+[ ,]+)?(?:please +)?(?:(?:can|could|would|will) you +)?(?:please +)?)`)
	buildVerb      = regexp.MustCompile(`^(?:build|make|create|design|draw|change|edit|update|fix|redesign|give|add|remove|paint|color|colour)\b`)
	buildNew       = regexp.MustCompile(`^(?:build|make|create|design|draw)\b`)
	buildFace      = regexp.MustCompile(`\b(?:your|my cat'?s|the cat'?s|this cat'?s) (?:face|look|eyes?|nose|ears?|mouth|whiskers|cheeks?|stripes|smile|fur|eyebrows?)\b|^give yourself\b.*\b(?:face|eyes?|nose|ears?|mouth|whiskers|cheeks?|stripes|smile|mustache|moustache|eyebrows?)\b`)
	buildApp       = regexp.MustCompile(`\b(?:apps?|games?|quiz(?:zes)?|drawing|storybook|flash[ -]?cards|memory match|tic[ -]tac[ -]toe)\b`)
	buildNewObject = regexp.MustCompile(`^(?:build|make|create|design) (?:me |us )?(?:(?:a|an|another|new|some) (?:[a-z0-9-]+ ){0,8})?(?:app|game|quiz|drawing|storybook|flash[ -]?cards|memory match|tic[ -]tac[ -]toe)\b|^draw (?:me |us )?(?:a|an|some) `)
	buildModifier  = regexp.MustCompile(` (?:about|with|where|that|to|for|and) `)
	buildSimple    = regexp.MustCompile(`^(?:make|change|edit|turn) (?:my |the |your )?(?:volume|voice|brightness|name|face color|face colour)\b`)
	buildBareApp   = regexp.MustCompile(`^(?:build|make|create|design) (?:me |us )?(?:a |an |new )?(?:app|game|quiz|drawing)$`)
	buildBareFace  = regexp.MustCompile(`^(?:change|edit|fix|redesign|update) (?:your|my cat'?s|the cat'?s|this cat'?s) (?:face|look|eyes?|nose|ears?|mouth|whiskers|cheeks?|stripes|smile|fur|eyebrows?)$`)
	buildAmbiguous = regexp.MustCompile(`^(?:build|make|create|design|change|edit|update|fix|redesign) (?:me )?(?:it|something|stuff)(?: |$)`)
	buildCancel    = regexp.MustCompile(`^(?:cancel|stop) (?:the |my |this )?(?:background (?:build|job)|build|builder|app build|face (?:edit|build))(?: please)?$`)
	buildTarget    = regexp.MustCompile("`([a-z0-9]+(?:-[a-z0-9]+)*)`")
)

func parseBuildIntent(text string) buildIntent {
	s := strings.ToLower(strings.TrimSpace(text))
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(buildLead.ReplaceAllString(s, ""), ".!? ")
	switch {
	case buildCancel.MatchString(s):
		return buildIntent{kind: "cancel"}
	case !buildVerb.MatchString(s) || buildSimple.MatchString(s):
		return buildIntent{}
	case buildBareApp.MatchString(s):
		return buildIntent{kind: "clarify-app"}
	case buildBareFace.MatchString(s):
		return buildIntent{kind: "clarify-face"}
	case buildFace.MatchString(s):
		return buildIntent{kind: "face"}
	case buildNew.MatchString(s) && buildNewObject.MatchString(buildModifier.Split(s, 2)[0]):
		return buildIntent{kind: "new"}
	case !buildNew.MatchString(s) && buildApp.MatchString(s) || buildApp.MatchString(buildModifier.Split(s, 2)[0]):
		target := ""
		if match := buildTarget.FindStringSubmatch(s); match != nil {
			target = match[1]
		}
		return buildIntent{"edit", target}
	case buildAmbiguous.MatchString(s):
		return buildIntent{kind: "clarify"}
	default:
		return buildIntent{}
	}
}
