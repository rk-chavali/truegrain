WITH "fact_1" AS (
  SELECT
    date_trunc('month', "orders"."order_date") AS "order_date",
    SUM("orders"."order_total") AS "order_revenue"
  FROM "main"."orders" AS "orders"
  GROUP BY 1
),
"fact_2" AS (
  SELECT
    date_trunc('month', "orders"."order_date") AS "order_date",
    SUM("campaigns"."spend") AS "campaign_spend"
  FROM "main"."campaigns" AS "campaigns"
    LEFT JOIN "main"."orders" AS "orders" ON "campaigns"."order_id" = "orders"."order_id"
  GROUP BY 1
)
SELECT
  COALESCE("fact_1"."order_date", "fact_2"."order_date") AS "order_date",
  "fact_1"."order_revenue" AS "order_revenue",
  "fact_2"."campaign_spend" AS "campaign_spend"
FROM "fact_1"
  FULL OUTER JOIN "fact_2" ON ("fact_1"."order_date" = "fact_2"."order_date" OR ("fact_1"."order_date" IS NULL AND "fact_2"."order_date" IS NULL))
LIMIT 24
