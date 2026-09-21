package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"ordersvc/internal/domain"
	"ordersvc/internal/usecase"
)

var errExtraJSONValue = errors.New("extra JSON value")

type Handler struct {
	createOrder usecase.CreateOrderUsecase
}

func New(createOrder usecase.CreateOrderUsecase) *Handler {
	return &Handler{createOrder: createOrder}
}

type createOrderRequest struct {
	CustomerID   string                   `json:"customer_id"`
	CustomerTier usecase.CustomerTier     `json:"customer_tier"`
	Items        []createOrderRequestItem `json:"items"`
}

type createOrderRequestItem struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
	UnitPrice int64  `json:"unit_price_cents"`
}

type orderResponse struct {
	ID     string             `json:"id"`
	Status domain.OrderStatus `json:"status"`
}

func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	var request createOrderRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := ensureEOF(decoder); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body must contain one JSON object"})
		return
	}

	items := make([]domain.OrderItem, len(request.Items))
	for i, item := range request.Items {
		items[i] = domain.OrderItem{
			ProductID: item.ProductID,
			Quantity:  item.Quantity,
			UnitPrice: item.UnitPrice,
		}
	}
	order, err := h.createOrder.Execute(r.Context(), &usecase.CreateOrderRequest{
		CustomerID:   request.CustomerID,
		CustomerTier: request.CustomerTier,
		Items:        items,
	})
	if err != nil {
		switch {
		case (errors.Is(err, usecase.ErrPaymentUncertain) || errors.Is(err, usecase.ErrFulfillmentPending)) && order != nil:
			writeJSON(w, http.StatusAccepted, orderResponse{ID: order.ID, Status: order.Status})
		case errors.Is(err, usecase.ErrPaymentRejected) && order != nil:
			writeJSON(w, http.StatusBadGateway, orderResponse{ID: order.ID, Status: order.Status})
		case isValidationError(err):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid order"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create order"})
		}
		return
	}

	status := http.StatusCreated
	if order.Status == domain.OrderStatusFailed {
		status = http.StatusPaymentRequired
	}
	writeJSON(w, status, orderResponse{ID: order.ID, Status: order.Status})
}

func isValidationError(err error) bool {
	return errors.Is(err, usecase.ErrNilRequest) ||
		errors.Is(err, usecase.ErrInvalidCustomerTier) ||
		errors.Is(err, domain.ErrEmptyCustomer) ||
		errors.Is(err, domain.ErrEmptyOrder) ||
		errors.Is(err, domain.ErrInvalidOrderItem)
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errExtraJSONValue
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
