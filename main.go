package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os" // <- TAMBAH INI
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
)

// ===================== Struct request dari client =====================

type Passenger struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Identity       string `json:"identity"`
	IdentityNumber string `json:"identity_number"`
	TicketPrice    int    `json:"ticket_price"` // akan di-override dari total_price
}

type AutoBookRequest struct {
	Asal        int         `json:"asal"`
	Tujuan      int         `json:"tujuan"`
	Tanggal     string      `json:"tanggal"`      // 25-11-2025
	PaymentType string      `json:"payment_type"` // "VA"
	TicketType  string      `json:"ticket_type"`  // "Pergi"
	Passengers  []Passenger `json:"passengers"`   // ticket_price diabaikan, nanti diganti
}

// ... (SEMUA STRUCT LAINNYA TETAP SAMA) ...

const (
	apiToken             = "79580|u16uKHzi7A3xdm0Jojwwxd7os01Yl5lXQJfH6btQ"
	schedulesURL         = "https://jaketboat.bankdki.co.id/api/v1/schedules"
	bookingURL           = "https://jaketboat.bankdki.co.id/api/v1/booking"
	createBillingURL     = "http://118.99.71.122:8443/vadkipelabuhan-prod/v1/transaksi/createbilling"
	updatePaymentCodeURL = "https://jaketboat.bankdki.co.id/api/v1/payment/update-code"
	requestTimeout       = 15 * time.Second
)

func main() {
	app := fiber.New()

	// CORS masih OK, tapi karena index.html nanti diserve dari server yang sama,
	// sebenarnya CORS jarang kepakai. Gak apa-apa dibiarkan.
	app.Use(cors.New())

	// Serve index.html di root
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendFile("index.html")
	})

	// Kalau nanti punya file2 static lain (JS/CSS terpisah), bisa:
	// app.Static("/", "./")

	// Endpoint utama dari HTML
	app.Post("/auto-book", handleAutoBook)

	// BACA PORT dari environment (WAJIB di Railway)
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000" // default kalau running lokal
	}

	log.Println("Server running on :" + port)
	if err := app.Listen(":" + port); err != nil {
		log.Fatal(err)
	}
}

// ===================== /schedules =====================

type ScheduleItem struct {
	ID            int `json:"id"`
	TotalPrice    int `json:"total_price"`
	TiketTersedia int `json:"tiket_tersedia"`
	BiayaAdmin    int `json:"biaya_admin"`
}

type ScheduleResponse struct {
	Success bool           `json:"success"`
	Message string         `json:"message"`
	Data    []ScheduleItem `json:"data"`
}

// ===================== /booking =====================

type ExternalBookingItem struct {
	ScheduleID int         `json:"schedule_id"`
	TicketType string      `json:"ticket_type"`
	Passengers []Passenger `json:"passengers"`
}

type ExternalBookingRequest struct {
	PaymentType string                `json:"payment_type"`
	Bookings    []ExternalBookingItem `json:"bookings"`
}

type BookingAPIResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		PaymentID int    `json:"payment_id"`
		RefNumber string `json:"ref_number"`
	} `json:"data"`
}

// ===================== /createbilling (VA) =====================

type CreateBillingResponse struct {
	Status  bool   `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		NoRef          string `json:"no_ref"`
		IDKey          string `json:"id_key"`
		IDTagihan      string `json:"id_tagihan"`
		Amount         string `json:"amount"`
		TanggalExpired string `json:"tanggal_expired"` // "2025-11-24 13:06:09"
		VANumber       string `json:"va_number"`       // "9910035154803722"
	} `json:"data"`
}

// ===================== /payment/update-code =====================

type UpdateCodeRequest struct {
	PaymentID   int    `json:"payment_id"`
	PaymentCode string `json:"payment_code"`
	TimeToPay   string `json:"time_to_pay"` // "24/11/2025 23:06:09"
	InvoiceID   string `json:"invoice_id"`
}

type UpdateCodeResponse map[string]interface{}

// ===================== Result gabungan per schedule =====================

type BookingResult struct {
	ScheduleID        int                    `json:"schedule_id"`
	Price             int                    `json:"price"`
	Booking           *BookingAPIResponse    `json:"booking,omitempty"`
	Billing           *CreateBillingResponse `json:"billing,omitempty"`
	UpdatePaymentCode UpdateCodeResponse     `json:"update_payment_code,omitempty"`
	BookingError      string                 `json:"booking_error,omitempty"`
	BillingError      string                 `json:"billing_error,omitempty"`
	UpdateError       string                 `json:"update_error,omitempty"`
}

func handleAutoBook(c *fiber.Ctx) error {
	var req AutoBookRequest

	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"success": false,
			"message": "invalid request body",
			"error":   err.Error(),
		})
	}

	// 1. Ambil schedules
	schedules, _, err := getSchedules(req.Asal, req.Tujuan, req.Tanggal)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"success": false,
			"message": "failed to get schedules",
			"error":   err.Error(),
		})
	}
	if len(schedules) == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success": false,
			"message": "no schedules found",
		})
	}

	results := make([]BookingResult, 0, len(schedules))
	passengerCount := len(req.Passengers)
	allFailed := true

	// 2. Loop per schedule → cek tiket_tersedia → booking → (jika sukses) createbilling → (jika sukses) update-code
	for _, s := range schedules {
		res := BookingResult{
			ScheduleID: s.ID,
			Price:      s.TotalPrice,
		}

		// 2a. Cek stok tiket dulu
		if s.TiketTersedia < passengerCount {
			msg := fmt.Sprintf("not enough tickets: available=%d, passengers=%d", s.TiketTersedia, passengerCount)
			log.Printf("Skip schedule %d: %s\n", s.ID, msg)
			res.BookingError = msg
			results = append(results, res)
			continue
		}

		// override ticket_price dari total_price
		passengersWithPrice := make([]Passenger, passengerCount)
		for i, p := range req.Passengers {
			p.TicketPrice = s.TotalPrice
			passengersWithPrice[i] = p
		}

		// 2b. Booking ke jaketboat
		bookingReq := ExternalBookingRequest{
			PaymentType: req.PaymentType,
			Bookings: []ExternalBookingItem{
				{
					ScheduleID: s.ID,
					TicketType: req.TicketType,
					Passengers: passengersWithPrice,
				},
			},
		}

		bookingResp, err := doBooking(bookingReq)
		if err != nil {
			log.Printf("Booking error for schedule %d: %v\n", s.ID, err)
			res.BookingError = err.Error()
			results = append(results, res)
			continue
		}
		res.Booking = &bookingResp

		if !bookingResp.Success {
			log.Printf("Booking failed for schedule %d: %s\n", s.ID, bookingResp.Message)
			res.BookingError = bookingResp.Message
			results = append(results, res)
			continue
		}

		// Booking berhasil, set allFailed = false
		allFailed = false

		paymentID := bookingResp.Data.PaymentID
		log.Printf("Schedule %d booked, payment_id=%d\n", s.ID, paymentID)

		amount := s.TotalPrice*len(req.Passengers) + s.BiayaAdmin
		billingResp, err := createBilling(amount)

		// 2c. Create billing ke VA server
		if err != nil {
			log.Printf("Create billing error for schedule %d: %v\n", s.ID, err)
			res.BillingError = err.Error()
			results = append(results, res)
			continue
		}
		res.Billing = &billingResp

		if !billingResp.Status || billingResp.Code != "00" {
			log.Printf("Create billing failed for schedule %d: %s\n", s.ID, billingResp.Message)
			res.BillingError = billingResp.Message
			results = append(results, res)
			continue
		}

		vaNumber := billingResp.Data.VANumber
		invoiceID := billingResp.Data.IDTagihan
		expiredRaw := billingResp.Data.TanggalExpired // "2025-11-24 13:06:09"

		// Convert waktu: 2006-01-02 15:04:05 -> 02/01/2006 15:04:05
		timeToPay, err := convertTimeFormat(expiredRaw)
		if err != nil {
			log.Printf("Time parse error for schedule %d: %v\n", s.ID, err)
			res.BillingError = "failed to parse tanggal_expired: " + err.Error()
			results = append(results, res)
			continue
		}

		log.Printf("Billing: schedule=%d va=%s invoice=%s time_to_pay=%s\n",
			s.ID, vaNumber, invoiceID, timeToPay)

		// 2d. Update payment code di jaketboat
		updateResp, err := updatePaymentCode(UpdateCodeRequest{
			PaymentID:   paymentID,
			PaymentCode: vaNumber,
			TimeToPay:   timeToPay,
			InvoiceID:   invoiceID,
		})
		if err != nil {
			log.Printf("Update payment code error for schedule %d: %v\n", s.ID, err)
			res.UpdateError = err.Error()
			results = append(results, res)
			continue
		}

		res.UpdatePaymentCode = updateResp
		results = append(results, res)
	}

	// 3. Cek apakah semua schedule gagal
	if allFailed {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"success":         false,
			"message":         "all schedules failed to book",
			"booking_results": results,
		})
	}

	// 4. Balik ke client – semua schedule sudah diproses (sukses/gagal per item)
	return c.JSON(fiber.Map{
		"success":         true,
		"message":         "processed all schedules, check booking_results for status per schedule",
		"booking_results": results,
	})
}

// ===================== Helper: /schedules =====================

func getSchedules(asal, tujuan int, tanggal string) ([]ScheduleItem, *ScheduleResponse, error) {
	bodyStruct := struct {
		Asal    int    `json:"asal"`
		Tujuan  int    `json:"tujuan"`
		Tanggal string `json:"tanggal"`
	}{
		Asal:    asal,
		Tujuan:  tujuan,
		Tanggal: tanggal,
	}

	jsonBody, err := json.Marshal(bodyStruct)
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequest(http.MethodPost, schedulesURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, nil, err
	}

	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	var schedResp ScheduleResponse
	if err := json.NewDecoder(resp.Body).Decode(&schedResp); err != nil {
		return nil, nil, err
	}

	for _, d := range schedResp.Data {
		log.Printf("Schedule ID: %d | total_price: %d | tiket_tersedia: %d\n",
			d.ID, d.TotalPrice, d.TiketTersedia)
	}

	return schedResp.Data, &schedResp, nil
}

// ===================== Helper: /booking =====================

func doBooking(bookingReq ExternalBookingRequest) (BookingAPIResponse, error) {
	var bookingResp BookingAPIResponse

	jsonBody, err := json.Marshal(bookingReq)
	if err != nil {
		return bookingResp, err
	}

	req, err := http.NewRequest(http.MethodPost, bookingURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return bookingResp, err
	}

	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return bookingResp, err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&bookingResp); err != nil {
		return bookingResp, err
	}

	return bookingResp, nil
}

// ===================== Helper: /createbilling =====================

func createBilling(amount int) (CreateBillingResponse, error) {
	var billingResp CreateBillingResponse

	form := url.Values{}
	form.Set("amount", strconv.Itoa(amount))
	form.Set("merchant_id", "PTPATTRA001")
	form.Set("notif_url", "https://jaketboat.bankdki.co.id/api/v1/payment/payout/va")
	form.Set("expired_param", "120") // sesuai contoh kamu

	req, err := http.NewRequest(http.MethodPost, createBillingURL, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return billingResp, err
	}

	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return billingResp, err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&billingResp); err != nil {
		return billingResp, err
	}

	return billingResp, nil
}

// ===================== Helper: /payment/update-code =====================

func updatePaymentCode(updateReq UpdateCodeRequest) (UpdateCodeResponse, error) {
	var updateResp UpdateCodeResponse

	jsonBody, err := json.Marshal(updateReq)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, updatePaymentCodeURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&updateResp); err != nil {
		return nil, err
	}

	return updateResp, nil
}

// ===================== Helper: format waktu =====================

func convertTimeFormat(raw string) (string, error) {
	// raw contoh: "2025-11-24 13:06:09"
	layoutIn := "2006-01-02 15:04:05"
	layoutOut := "02/01/2006 15:04:05"

	t, err := time.Parse(layoutIn, raw)
	if err != nil {
		return "", err
	}
	return t.Format(layoutOut), nil
}
