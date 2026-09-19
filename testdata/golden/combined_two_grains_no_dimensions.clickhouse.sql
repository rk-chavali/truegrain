WITH `fact_1` AS (
  SELECT
    SUM(`orders`.`order_total`) AS `order_revenue`
  FROM `main`.`orders` AS `orders`
),
`fact_2` AS (
  SELECT
    SUM(`order_lines`.`line_amount`) AS `line_revenue`
  FROM `main`.`order_lines` AS `order_lines`
)
SELECT
  `fact_1`.`order_revenue` AS `order_revenue`,
  `fact_2`.`line_revenue` AS `line_revenue`
FROM `fact_1`
  FULL OUTER JOIN `fact_2` ON TRUE
LIMIT 1000
