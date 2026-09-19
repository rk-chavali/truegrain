WITH "fact_1" AS (
  SELECT
    "customers"."region" AS "region",
    SUM("orders"."order_total") AS "order_revenue"
  FROM "main"."orders" AS "orders"
    LEFT JOIN "main"."customers" AS "customers" ON "orders"."customer_id" = "customers"."customer_id"
  GROUP BY 1
),
"fact_2" AS (
  SELECT
    "customers"."region" AS "region",
    SUM("order_lines"."line_amount") AS "line_revenue",
    SUM("order_lines"."quantity") AS "units_sold"
  FROM "main"."order_lines" AS "order_lines"
    LEFT JOIN "main"."orders" AS "orders" ON "order_lines"."order_id" = "orders"."order_id"
    LEFT JOIN "main"."customers" AS "customers" ON "orders"."customer_id" = "customers"."customer_id"
  GROUP BY 1
)
SELECT
  COALESCE("fact_1"."region", "fact_2"."region") AS "region",
  "fact_1"."order_revenue" AS "order_revenue",
  "fact_2"."line_revenue" AS "line_revenue",
  "fact_2"."units_sold" AS "units_sold"
FROM "fact_1"
  FULL OUTER JOIN "fact_2" ON ("fact_1"."region" = "fact_2"."region" OR ("fact_1"."region" IS NULL AND "fact_2"."region" IS NULL))
ORDER BY "region"
LIMIT 1000
