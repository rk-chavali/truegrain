SELECT
  `customers`.`region` AS `region`,
  `order_lines`.`item_id` AS `item_id`,
  SUM(`order_lines`.`line_amount`) AS `line_revenue`,
  SUM(`order_lines`.`quantity`) AS `units_sold`
FROM `main.order_lines` AS `order_lines`
  LEFT JOIN `main.orders` AS `orders` ON `order_lines`.`order_id` = `orders`.`order_id`
  LEFT JOIN `main.customers` AS `customers` ON `orders`.`customer_id` = `customers`.`customer_id`
GROUP BY 1, 2
LIMIT 50
