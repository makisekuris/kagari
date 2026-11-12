package agentctx

import (
	"encoding/json"

	"kagari/internal/personal"
)

var deliveryFactTools = [...]string{
	"get_message",
	"get_delivery",
	"list_deliveries",
	"stop_delivery",
	"resume_task_delivery",
	"resend_delivery",
	"cancel_task",
}

func isDeliveryFactTool(name string) bool {
	for _, candidate := range deliveryFactTools {
		if candidate == name {
			return true
		}
	}
	return false
}

func compactDeliveryToolResult(name, raw, archiveReference string) (string, bool, error) {
	if name == "get_message" {
		var record personal.RecordText
		if err := json.Unmarshal([]byte(raw), &record); err != nil || record.ID <= 0 || record.Kind != "message" {
			return "", false, nil
		}
		omitted := record.Page.Text != ""
		record.Page.Text = ""
		if record.Delivery != nil {
			card := personal.DeliveryCard(*record.Delivery)
			record.Delivery = &card
			omitted = omitted || card.ContentOmitted
		}
		compact := struct {
			personal.RecordText
			ContentOmitted       bool   `json:"content_omitted,omitempty"`
			ArchivedToolMaterial string `json:"archived_tool_material,omitempty"`
		}{RecordText: record, ContentOmitted: omitted, ArchivedToolMaterial: archiveReference}
		encoded, err := json.Marshal(compact)
		return string(encoded), true, err
	}

	var value map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &value); err != nil || value == nil {
		return "", false, nil
	}
	if encoded, ok, err := compactDeliveryField(value, "delivery"); err != nil {
		return "", false, err
	} else if ok {
		value["delivery"] = encoded
	}
	if encoded, ok, err := compactDeliveryPageField(value, "deliveries"); err != nil {
		return "", false, err
	} else if ok {
		value["deliveries"] = encoded
	}
	if archiveReference != "" {
		encoded, err := json.Marshal(archiveReference)
		if err != nil {
			return "", false, err
		}
		value["archived_tool_material"] = encoded
	}
	encoded, err := json.Marshal(value)
	return string(encoded), true, err
}

func compactDeliveryField(value map[string]json.RawMessage, field string) (json.RawMessage, bool, error) {
	raw, ok := value[field]
	if !ok {
		return nil, false, nil
	}
	var intent personal.DeliveryIntent
	if err := json.Unmarshal(raw, &intent); err != nil || intent.ID <= 0 {
		return nil, false, nil
	}
	card := personal.DeliveryCard(intent)
	encoded, err := json.Marshal(card)
	return encoded, true, err
}

func compactDeliveryPageField(value map[string]json.RawMessage, field string) (json.RawMessage, bool, error) {
	raw, ok := value[field]
	if !ok {
		return nil, false, nil
	}
	var page personal.DeliveryPage
	if err := json.Unmarshal(raw, &page); err != nil || page.Items == nil {
		return nil, false, nil
	}
	for i := range page.Items {
		page.Items[i] = personal.DeliveryCard(page.Items[i])
	}
	encoded, err := json.Marshal(page)
	return encoded, true, err
}
