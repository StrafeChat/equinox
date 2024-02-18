import { Router } from "express";

const router = Router();
router.get("/worker.js", (req, res) => {
  res.sendFile("../static/worker.js");
});

export default router;