// Generated from the .json file of the same name. Do not edit.
globalThis.MOLDLOFT_EXAMPLES = [
  {
    "id": "all_met_with_na",
    "label": "Required summary present, transition schedule not applicable, advisory glossary omitted",
    "answers": {
      "plain_glossary": "no",
      "public_summary": "yes",
      "transition_plan": "na"
    },
    "expect": {
      "status": "complete",
      "unmet_required": 0,
      "yes_count": 1,
      "no_count": 1,
      "na_count": 1
    }
  },
  {
    "id": "missing_required_item",
    "label": "Required summary missing leaves one unmet required item",
    "answers": {
      "plain_glossary": "yes",
      "public_summary": "no",
      "transition_plan": "yes"
    },
    "expect": {
      "status": "incomplete",
      "unmet_required": 1,
      "yes_count": 2,
      "no_count": 1,
      "na_count": 0
    }
  }
];
