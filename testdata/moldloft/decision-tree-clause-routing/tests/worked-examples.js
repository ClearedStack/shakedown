// Generated from the .json file of the same name. Do not edit.
globalThis.MOLDLOFT_EXAMPLES = [
  {
    "id": "standard_micro",
    "label": "Micro-purchase without hazard endorsement",
    "answers": {
      "supplies_or_services": "no",
      "under_micro_limit": "yes"
    },
    "expect": {
      "outcome": "micro_purchase_ok",
      "status": "eligible",
      "steps": 2
    }
  },
  {
    "id": "hazard_micro",
    "label": "Micro-purchase with hazard endorsement",
    "answers": {
      "supplies_or_services": "yes",
      "under_micro_limit": "yes"
    },
    "expect": {
      "outcome": "specialist_review",
      "status": "review",
      "steps": 2
    }
  },
  {
    "id": "simplified_order",
    "label": "Order between micro-purchase and simplified limits",
    "answers": {
      "under_micro_limit": "no",
      "under_simplified_limit": "yes"
    },
    "expect": {
      "outcome": "simplified_quote_ok",
      "status": "eligible",
      "steps": 2
    }
  },
  {
    "id": "above_simplified",
    "label": "Order above simplified limit",
    "answers": {
      "under_micro_limit": "no",
      "under_simplified_limit": "no"
    },
    "expect": {
      "outcome": "formal_solicitation_required",
      "status": "ineligible",
      "steps": 2
    }
  }
];
