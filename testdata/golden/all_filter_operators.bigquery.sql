SELECT
  `customers`.`region` AS `region`,
  SUM(`orders`.`order_total`) AS `order_revenue`
FROM `main.orders` AS `orders`
  LEFT JOIN `main.customers` AS `customers` ON `orders`.`customer_id` = `customers`.`customer_id`
WHERE `orders`.`status` IN (@p1, @p2)
  AND `customers`.`region` <> @p3
  AND `orders`.`order_date` BETWEEN @p4 AND @p5
  AND `customers`.`signup_date` IS NOT NULL
GROUP BY 1
LIMIT 1000

-- parameters:
--   $1 = "shipped"
--   $2 = "delivered"
--   $3 = "XX"
--   $4 = plan.Date{Year:2026, Month:1, Day:1}
--   $5 = plan.Date{Year:2026, Month:12, Day:31}
