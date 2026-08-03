# Document 7: API Contract Design (OpenAPI 3.0 Specification)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** API
**Traces From:** [SRS](02_SRS_System_Requirements.md) → [MDS](04_MDS_Module_Design.md) → [DDS](05_DDS_Detailed_Design.md)
**Traces To:** [TST](13_TST_Test_Strategy.md)
**New in v2:** Subscriber health endpoint (FR-OBS-004), notification log endpoints (FR-NOTIF-009), revenue assurance endpoints (FR-REV-001..004), subscriber portal endpoints (FR-SUB-001..005), franchise/LCO endpoints (FR-FRN-001..003), WhatsApp delivery webhook (FR-NOTIF-011), GSTR-1 export (FR-BIL-006)

---

```yaml
openapi: 3.0.3
info:
  title: BSS/OSS Core ISP Management Engine API
  version: 1.0.0
  description: >
    Type-safe operational routes governing subscriber lifecycle, wallet systems,
    session management, ticketing, and LEA export.
    All routes except /health require a valid JWT Bearer token.

components:
  securitySchemes:
    BearerAuth:
      type: http
      scheme: bearer
      bearerFormat: JWT

  schemas:
    Error:
      type: object
      required: [code, message]
      properties:
        code:
          type: string
          example: "ERR_SUBSCRIBER_NOT_FOUND"
        message:
          type: string
          example: "Subscriber with ID 42 not found."
        correlation_id:
          type: string
          description: Trace ID for log correlation

    Subscriber:
      type: object
      properties:
        id:
          type: integer
        caf_number:
          type: string
        username:
          type: string
        mobile_number:
          type: string
        status:
          type: string
          enum: [active, grace_period, soft_suspended, hard_suspended, terminated]
        plan_id:
          type: integer
        plan_expiry:
          type: string
          format: date-time
        wallet_balance:
          type: string
          description: Decimal string to avoid float precision loss
        registered_state:
          type: string
        kyc_status:
          type: string
          enum: [pending, verified, rejected]
        created_at:
          type: string
          format: date-time

security:
  - BearerAuth: []

paths:

  /health:
    get:
      summary: Health check
      security: []
      responses:
        '200':
          description: Service is healthy

  # ── Subscribers ──────────────────────────────────────────────────────────────

  /api/v1/subscribers:
    post:
      summary: Register new subscriber (CAF entry)
      description: >
        Creates subscriber record. PII fields (Aadhaar, PAN) are encrypted at the
        application layer before persistence. Required role: billing_admin.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [caf_number, username, mobile_number, plan_id, registered_state]
              properties:
                caf_number:
                  type: string
                username:
                  type: string
                password:
                  type: string
                  description: Plain text; server stores bcrypt hash only
                mobile_number:
                  type: string
                  pattern: '^\+91\d{10}$'
                  description: E.164 format
                email:
                  type: string
                plan_id:
                  type: integer
                registered_state:
                  type: string
                aadhaar:
                  type: string
                  description: Encrypted at application layer before storage
                pan:
                  type: string
                  description: Encrypted at application layer before storage
      responses:
        '201':
          description: Subscriber created; PII encrypted and persisted
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Subscriber'
        '409':
          description: CAF number or username already exists
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'
        '422':
          description: Validation error (invalid mobile format, missing required fields)
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'

  /api/v1/subscribers/{id}:
    get:
      summary: Get subscriber profile
      description: Required roles: noc_engineer, billing_admin, csr, technician
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: integer
      responses:
        '200':
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Subscriber'
        '404':
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'

    patch:
      summary: Update subscriber plan or status
      description: Required role: billing_admin
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                plan_id:
                  type: integer
                status:
                  type: string
                  enum: [active, terminated]
      responses:
        '200':
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Subscriber'

  # ── Wallets ───────────────────────────────────────────────────────────────────

  /api/v1/wallets/recharge:
    post:
      summary: Credit subscriber wallet (atomic double-entry)
      description: >
        Webhook HMAC signature must be validated by the caller before invoking this
        endpoint for gateway-originated credits. Idempotent: duplicate transaction_token
        returns the original transaction without re-crediting.
        Required role: billing_admin.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [subscriber_id, amount, payment_method, transaction_token]
              properties:
                subscriber_id:
                  type: integer
                amount:
                  type: string
                  description: Decimal string e.g. "799.00"
                payment_method:
                  type: string
                  enum: [razorpay, bbps, cash, manual]
                transaction_token:
                  type: string
                  description: Idempotency key; must be globally unique per transaction
      responses:
        '200':
          description: Wallet credited (or original transaction returned on duplicate token)
        '409':
          description: Duplicate transaction_token with different amount — conflict
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'

  /api/v1/wallets/{subscriber_id}/ledger:
    get:
      summary: Fetch wallet ledger entries
      description: Required roles: billing_admin, csr
      parameters:
        - name: subscriber_id
          in: path
          required: true
          schema:
            type: integer
        - name: from
          in: query
          schema:
            type: string
            format: date-time
        - name: to
          in: query
          schema:
            type: string
            format: date-time
        - name: limit
          in: query
          schema:
            type: integer
            default: 50
            maximum: 200
      responses:
        '200':
          description: Paginated ledger entries

  # ── Sessions ──────────────────────────────────────────────────────────────────

  /api/v1/sessions/{subscriber_id}/active:
    get:
      summary: Get active session for subscriber
      description: Required roles: noc_engineer, csr, technician
      parameters:
        - name: subscriber_id
          in: path
          required: true
          schema:
            type: integer
      responses:
        '200':
          description: Active session details including assigned IP, NAS IP, usage bytes
        '404':
          description: No active session found

  /api/v1/sessions/{session_id}/disconnect:
    post:
      summary: Issue PoD to forcibly disconnect a session
      description: >
        Enqueues an Asynq PoD task with retry logic.
        Required role: noc_engineer.
      parameters:
        - name: session_id
          in: path
          required: true
          schema:
            type: string
      responses:
        '202':
          description: PoD task enqueued; result is asynchronous

  /api/v1/sessions/{session_id}/fup-override:
    post:
      summary: Manually apply or remove FUP throttle on an active session
      description: Required role: noc_engineer
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [action]
              properties:
                action:
                  type: string
                  enum: [apply, remove]
      responses:
        '202':
          description: CoA task enqueued

  # ── Invoices ──────────────────────────────────────────────────────────────────

  /api/v1/invoices/{subscriber_id}:
    get:
      summary: List invoices for subscriber
      description: Required roles: billing_admin, csr
      parameters:
        - name: subscriber_id
          in: path
          required: true
          schema:
            type: integer
      responses:
        '200':
          description: List of invoice summaries

  /api/v1/invoices/{invoice_id}/pdf:
    get:
      summary: Download GST-compliant invoice PDF
      description: Required roles: billing_admin, csr, subscriber (self-service)
      parameters:
        - name: invoice_id
          in: path
          required: true
          schema:
            type: integer
      responses:
        '200':
          description: PDF binary
          content:
            application/pdf:
              schema:
                type: string
                format: binary
        '404':
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Error'

  # ── Tickets ───────────────────────────────────────────────────────────────────

  /api/v1/tickets:
    post:
      summary: Create support ticket
      description: Required roles: csr, subscriber (self-service)
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [subscriber_id, category, description]
              properties:
                subscriber_id:
                  type: integer
                category:
                  type: string
                  enum: [connectivity, billing, plan_change, other]
                description:
                  type: string
      responses:
        '201':
          description: Ticket created

  /api/v1/tickets/{ticket_id}:
    patch:
      summary: Update ticket status or assign technician
      description: Required roles: csr, technician
      responses:
        '200':
          description: Ticket updated

  # ── LEA Export ────────────────────────────────────────────────────────────────

  /api/v1/lea/lookup:
    post:
      summary: LEA IP-to-subscriber lookup
      description: >
        Returns subscriber identity for a given public IP + port + timestamp tuple.
        Every invocation writes a tamper-evident audit record to lea_audit_log.
        Required role: noc_engineer with lea_access permission flag.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [public_ip, timestamp]
              properties:
                public_ip:
                  type: string
                  format: ipv4
                port:
                  type: integer
                  description: Optional; required if CGNAT in use
                timestamp:
                  type: string
                  format: date-time
      responses:
        '200':
          description: Subscriber identity record
        '403':
          description: Caller lacks lea_access permission
        '404':
          description: No subscriber found for given tuple

  # ── CGNAT ─────────────────────────────────────────────────────────────────────

  /api/v1/cgnat/allocations/{subscriber_id}:
    get:
      summary: Get CGNAT port-block allocation history for subscriber
      description: Required role: noc_engineer
      parameters:
        - name: subscriber_id
          in: path
          required: true
          schema:
            type: integer
      responses:
        '200':
          description: List of CGNAT allocation records

  # ── Webhooks (inbound) ────────────────────────────────────────────────────────

  /webhooks/razorpay:
    post:
      summary: Razorpay payment webhook receiver
      description: >
        HMAC-SHA256 signature validated from X-Razorpay-Signature header
        against raw request body before any state mutation.
      security: []
      responses:
        '200':
          description: Webhook processed
        '400':
          description: Invalid HMAC signature

  # ── WhatsApp delivery webhook (new — FR-NOTIF-011) ───────────────────────────

  /webhooks/whatsapp:
    get:
      summary: Meta webhook verification challenge
      security: []
      parameters:
        - name: hub.mode
          in: query
          schema: { type: string }
        - name: hub.verify_token
          in: query
          schema: { type: string }
        - name: hub.challenge
          in: query
          schema: { type: string }
      responses:
        '200':
          description: Echo hub.challenge if verify_token matches
    post:
      summary: WhatsApp delivery status callback
      description: >
        Receives sent/delivered/read/failed status events from Meta.
        Verified via X-Hub-Signature-256 header.
        Updates notification_log.delivery_status (FR-NOTIF-011).
      security: []
      responses:
        '200':
          description: Status processed

  # ── Subscriber health (new — FR-OBS-004) ─────────────────────────────────────

  /api/v1/subscribers/{id}/health:
    get:
      summary: Single-call subscriber diagnostic view for CSR and NOC
      description: >
        Aggregates Redis session state + DB metadata. Designed to answer a
        subscriber complaint call in under 30 seconds.
        Required roles: noc_engineer, csr, technician.
      parameters:
        - name: id
          in: path
          required: true
          schema: { type: integer }
      responses:
        '200':
          description: Subscriber health summary
          content:
            application/json:
              schema:
                type: object
                properties:
                  subscriber_id: { type: integer }
                  username: { type: string }
                  status: { type: string }
                  wallet_balance: { type: string }
                  plan_expiry: { type: string, format: date-time }
                  active_session:
                    type: object
                    nullable: true
                    properties:
                      session_id: { type: string }
                      nas_ip: { type: string }
                      assigned_ip: { type: string }
                      bytes_used: { type: integer }
                      bytes_total: { type: integer }
                      pct_used: { type: integer }
                      speed_profile: { type: string }
                      session_age: { type: string }
                  fup_status: { type: string, enum: [below, warning, throttled] }
                  last_coa_result: { type: string, enum: [ack, nak, pending, none] }
                  open_tickets: { type: integer }
                  last_notification:
                    type: object
                    nullable: true
                    properties:
                      channel: { type: string }
                      event: { type: string }
                      sent_at: { type: string, format: date-time }
                      delivery_status: { type: string }

  # ── Subscriber usage (new — FR-SUB-001, used by portal) ──────────────────────

  /api/v1/subscribers/{id}/usage:
    get:
      summary: Real-time data usage for subscriber portal (reads from Redis)
      description: Required roles: billing_admin, csr, or subscriber-scoped JWT.
      parameters:
        - name: id
          in: path
          required: true
          schema: { type: integer }
      responses:
        '200':
          description: Current session usage
          content:
            application/json:
              schema:
                type: object
                properties:
                  gb_used: { type: number }
                  gb_total: { type: integer }
                  pct_used: { type: integer }
                  fup_status: { type: string }
                  speed_profile: { type: string }

  # ── Notification log (new — FR-NOTIF-009, FR-SUB-005) ────────────────────────

  /api/v1/subscribers/{id}/notifications:
    get:
      summary: Notification delivery history for a subscriber
      description: >
        Returns all notification_log entries for the subscriber.
        Used by CSR to answer "why was I disconnected" calls and by subscriber portal.
        Required roles: billing_admin, csr, or subscriber-scoped JWT.
      parameters:
        - name: id
          in: path
          required: true
          schema: { type: integer }
        - name: limit
          in: query
          schema: { type: integer, default: 20, maximum: 100 }
      responses:
        '200':
          description: Notification log entries

  # ── Revenue assurance (new — FR-REV-001..004) ────────────────────────────────

  /api/v1/revenue/unbilled:
    get:
      summary: Unbilled active subscribers report
      description: >
        Lists active subscribers with no invoice in current billing cycle.
        Required role: billing_admin.
      responses:
        '200':
          description: Unbilled subscriber list with count and total at-risk revenue

  /api/v1/revenue/reconciliation:
    get:
      summary: Ledger reconciliation — wallet balance vs ledger net
      description: >
        Returns variance between subscriber wallet balances and ledger net.
        Should be zero. Required role: billing_admin.
      responses:
        '200':
          description: Reconciliation result with variance amount

  /api/v1/revenue/collections-forecast:
    get:
      summary: 30-day forward collections forecast
      description: >
        Segments subscribers into will-renew / at-risk / lapsed with expected revenue.
        Required role: billing_admin.
      parameters:
        - name: days
          in: query
          schema: { type: integer, default: 30, maximum: 90 }
      responses:
        '200':
          description: Collections forecast by segment

  /api/v1/revenue/gstr1-export:
    get:
      summary: GSTR-1 compatible GST export
      description: >
        Produces B2B/B2C invoice breakdown, HSN summary, and state-wise aggregate
        for the specified billing month. Required role: billing_admin.
      parameters:
        - name: month
          in: query
          required: true
          schema: { type: string, example: "2025-05" }
        - name: format
          in: query
          schema: { type: string, enum: [json, csv], default: json }
      responses:
        '200':
          description: GSTR-1 export data

  # ── Franchise / LCO (new — FR-FRN-001..003) ──────────────────────────────────

  /api/v1/franchises:
    get:
      summary: List all LCO / franchise partners
      description: Required role: billing_admin
      responses:
        '200':
          description: Franchise list

  /api/v1/franchises/{franchise_id}/pnl:
    get:
      summary: P&L summary for a specific LCO franchise
      description: Required role: billing_admin
      parameters:
        - name: franchise_id
          in: path
          required: true
          schema: { type: integer }
        - name: from
          in: query
          schema: { type: string, format: date }
        - name: to
          in: query
          schema: { type: string, format: date }
      responses:
        '200':
          description: LCO P&L with subscriber count, total recharges, commission earned

  /api/v1/franchises/consolidated-pnl:
    get:
      summary: Consolidated P&L across all LCO partners
      description: Required role: billing_admin
      responses:
        '200':
          description: Consolidated franchise P&L
```
