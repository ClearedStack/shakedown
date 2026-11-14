// Generated from the .json file of the same name. Do not edit.
globalThis.MOLDLOFT_TOOL = {
  "slug": "sample-clause-routing",
  "title": "Sample Purchase Threshold Decision Tree",
  "summary": "Routes a synthetic purchase request to the applicable threshold procedure based on published yes/no questions.",
  "scope_limits": [
    "Covers synthetic threshold routing only; does not address sole-source justifications or emergency authorities.",
    "Synthetic fixture rule set, not an active acquisition regulation."
  ],
  "citations": [
    {
      "id": "manual_part_13",
      "label": "Sample Threshold Routing Guide, Part 13.2",
      "note": "Synthetic public-domain routing rules used solely as a Moldloft test fixture."
    },
    {
      "id": "manual_part_15",
      "label": "Sample Threshold Routing Guide, Part 15.1",
      "note": "Synthetic public-domain formal solicitation rules."
    }
  ],
  "start": "under_micro_limit",
  "questions": [
    {
      "id": "under_micro_limit",
      "prompt": "Is the total estimated order value at or below the micro-purchase threshold?",
      "help": "Include base quantities and all priced delivery charges.",
      "citation": "manual_part_13",
      "options": [
        {
          "key": "yes",
          "label": "Yes, at or below micro-purchase limit",
          "next": "supplies_or_services"
        },
        {
          "key": "no",
          "label": "No, above micro-purchase limit",
          "next": "under_simplified_limit"
        }
      ]
    },
    {
      "id": "supplies_or_services",
      "prompt": "Does the order require a special hazard handling endorsement?",
      "citation": "manual_part_13",
      "options": [
        {
          "key": "no",
          "label": "No special hazard endorsement",
          "next": "micro_purchase_ok"
        },
        {
          "key": "yes",
          "label": "Yes, special hazard endorsement applies",
          "next": "specialist_review"
        }
      ]
    },
    {
      "id": "under_simplified_limit",
      "prompt": "Is the total estimated order value at or below the simplified threshold?",
      "citation": "manual_part_15",
      "options": [
        {
          "key": "yes",
          "label": "Yes, within simplified threshold",
          "next": "simplified_quote_ok"
        },
        {
          "key": "no",
          "label": "No, exceeds simplified threshold",
          "next": "formal_solicitation_required"
        }
      ]
    }
  ],
  "outcomes": [
    {
      "id": "micro_purchase_ok",
      "title": "Direct Micro-Purchase Procedure",
      "summary": "Eligible for direct micro-purchase ordering when price is considered reasonable.",
      "status": "eligible",
      "citation": "manual_part_13"
    },
    {
      "id": "specialist_review",
      "title": "Safety Specialist Review Required",
      "summary": "Requires written safety specialist concurrence before placing any order.",
      "status": "review",
      "citation": "manual_part_13"
    },
    {
      "id": "simplified_quote_ok",
      "title": "Simplified Quotation Procedure",
      "summary": "Eligible for simplified competitive quotation procedures.",
      "status": "eligible",
      "citation": "manual_part_15"
    },
    {
      "id": "formal_solicitation_required",
      "title": "Not Eligible for Simplified Procedure",
      "summary": "Exceeds simplified threshold; route to formal written solicitation.",
      "status": "ineligible",
      "citation": "manual_part_15"
    }
  ]
};
