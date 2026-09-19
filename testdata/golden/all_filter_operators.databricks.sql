SELECT
  `customers`.`region` AS `region`,
  SUM(`orders`.`order_total`) AS `order_revenue`
FROM `main`.`orders` AS `orders`
  LEFT JOIN `main`.`customers` AS `customers` ON `orders`.`customer_id` = `customers`.`customer_id`
WHERE `orders`.`status` IN (?, ?)
  AND `customers`.`region` <> ?
  AND `orders`.`order_date` BETWEEN ? AND ?
  AND `customers`.`signup_date` IS NOT NULL
GROUP BY 1
LIMIT 1000

-- parameters:
--   $1 = "shipped"
--   $2 = "delivered"
--   $3 = "XX"
--   $4 = plan.Date{Year:2026, Month:1, Day:1}
--   $5 = plan.Date{Year:2026, Month:12, Day:31}
