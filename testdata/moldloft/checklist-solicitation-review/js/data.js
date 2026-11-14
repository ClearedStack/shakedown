// Generated from the .json file of the same name. Do not edit.
globalThis.MOLDLOFT_TOOL = {
  "slug": "sample-solicitation-review",
  "title": "Sample solicitation preparation checklist",
  "summary": "Checks three made-up preparation steps before a fictional notice is filed. Every rule and citation in this tool is invented for testing.",
  "scope_limits": [
    "Every rule and citation here is fictional. Do not use this checklist for a real solicitation."
  ],
  "citations": [
    {
      "id": "rule_notice",
      "label": "Sample Rule 44.1(a) (fictional)",
      "note": "States that a made-up public notice summary must be attached",
      "url": "https://rules.example/sample-rule-44-1"
    },
    {
      "id": "rule_period",
      "label": "Sample Rule 44.2(b) (fictional)",
      "note": "States when the optional transition schedule applies"
    }
  ],
  "items": [
    {
      "id": "public_summary",
      "label": "Public summary sheet is attached",
      "kind": "yes_no",
      "required": true,
      "citation": "rule_notice",
      "help": "Required for every fictional filing."
    },
    {
      "id": "transition_plan",
      "label": "Transition schedule is attached",
      "kind": "yes_no",
      "required": true,
      "na_allowed": true,
      "citation": "rule_period",
      "help": "Mark Not applicable when there is no prior order."
    },
    {
      "id": "plain_glossary",
      "label": "Plain-language glossary is included",
      "kind": "yes_no",
      "required": false,
      "citation": "rule_notice"
    }
  ]
};
